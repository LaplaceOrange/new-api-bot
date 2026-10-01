package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsykk/new-api-bot/internal/qq"
	"github.com/fsykk/new-api-bot/internal/vendorstatus"
)

type vendorProgressQQ struct {
	*fakeQQ
	historyMu    sync.Mutex
	history      []string
	texts        []string
	textTimes    []time.Time
	sequences    []int
	replyIDs     []string
	recalled     []string
	imageStarted chan struct{}
	imageRelease chan struct{}
	imageErr     error
	textErrAt    int
	recallErr    bool
}

func (q *vendorProgressQQ) SendGroupTextWithSequence(ctx context.Context, _, replyID, text string, sequence int) (qq.SentMessage, error) {
	if err := ctx.Err(); err != nil {
		return qq.SentMessage{}, err
	}
	q.historyMu.Lock()
	defer q.historyMu.Unlock()
	index := len(q.texts) + 1
	q.texts = append(q.texts, text)
	q.textTimes = append(q.textTimes, time.Now())
	q.sequences = append(q.sequences, sequence)
	q.replyIDs = append(q.replyIDs, replyID)
	if q.textErrAt == index {
		q.history = append(q.history, "text-error")
		return qq.SentMessage{}, errors.New("QQ temporary send failure")
	}
	id := fmt.Sprintf("progress-%d", index)
	q.history = append(q.history, "text:"+id)
	return qq.SentMessage{ID: id}, nil
}

func (q *vendorProgressQQ) RecallGroupMessage(_ context.Context, _, id string) error {
	q.historyMu.Lock()
	defer q.historyMu.Unlock()
	q.history = append(q.history, "recall:"+id)
	if q.recallErr {
		return errors.New("QQ recall denied")
	}
	q.recalled = append(q.recalled, id)
	return nil
}

func (q *vendorProgressQQ) SendGroupFileWithSequence(ctx context.Context, _, replyID, _ string, _ int, _ []byte, sequence int) (qq.SentMessage, error) {
	q.historyMu.Lock()
	q.history = append(q.history, "image-start")
	q.sequences = append(q.sequences, sequence)
	q.replyIDs = append(q.replyIDs, replyID)
	q.historyMu.Unlock()
	if q.imageStarted != nil {
		close(q.imageStarted)
	}
	if q.imageRelease != nil {
		select {
		case <-q.imageRelease:
		case <-ctx.Done():
			return qq.SentMessage{}, ctx.Err()
		}
	}
	q.historyMu.Lock()
	q.history = append(q.history, "image-done")
	q.historyMu.Unlock()
	return qq.SentMessage{ID: "final-image"}, q.imageErr
}

func TestVendorProgressImmediateRotatesAndContinuesDuringUpload(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake, imageStarted: make(chan struct{}), imageRelease: make(chan struct{})}
	service.qq = q
	cfg := service.vendorBaseConfig()
	fetchRelease := make(chan struct{})
	firstTick := make(chan struct{})
	var once sync.Once
	service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		q.historyMu.Lock()
		if len(q.texts) == 0 {
			t.Error("query started before acknowledgement")
		}
		q.historyMu.Unlock()
		callbacks.Progress(vendorstatus.Packet{Stage: "fetch", Text: "正在收集 OpenAI 信息……", Completed: 2, Total: 20})
		once.Do(func() { close(firstTick) })
		select {
		case <-fetchRelease:
		case <-ctx.Done():
			return vendorstatus.Packet{}, ctx.Err()
		}
		callbacks.Progress(vendorstatus.Packet{Stage: "render", Text: "正在绘制20个来源"})
		return vendorstatus.Packet{PNG: []byte("png")}, nil
	}}
	done := make(chan error, 1)
	go func() {
		done <- service.runVendorQueryWithProgress(context.Background(), groupEvent("g", "u", "/vendor_status"), cfg, 5*time.Millisecond)
	}()
	<-firstTick
	awaitVendorProgress(t, func() bool {
		q.historyMu.Lock()
		defer q.historyMu.Unlock()
		for _, text := range q.texts {
			if strings.Contains(text, "正在收集 OpenAI") {
				return true
			}
		}
		return false
	})
	close(fetchRelease)
	<-q.imageStarted
	awaitVendorProgress(t, func() bool {
		q.historyMu.Lock()
		defer q.historyMu.Unlock()
		for _, text := range q.texts {
			if strings.Contains(text, "正在发送状态总览图片") {
				return true
			}
		}
		return false
	})
	close(q.imageRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	q.historyMu.Lock()
	defer q.historyMu.Unlock()
	if !strings.Contains(q.texts[0], "正在准备查询任务") || !strings.Contains(strings.Join(q.texts, "\n"), "2/20") {
		t.Fatal(q.texts)
	}
	lastID := fmt.Sprintf("progress-%d", len(q.texts))
	if len(q.recalled) != len(q.texts) || q.recalled[len(q.recalled)-1] != lastID {
		t.Fatal("last progress not cleaned", q.history)
	}
	for i, entry := range q.history {
		if strings.HasPrefix(entry, "recall:progress-") && i > 0 && i < len(q.history)-1 {
			if strings.HasPrefix(q.history[i-1], "recall:") {
				continue
			}
			if !strings.HasPrefix(q.history[i-1], "text:") && q.history[i-1] != "image-done" {
				t.Fatal("old progress retracted before replacement", q.history)
			}
		}
	}
}

func awaitVendorProgress(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("progress did not arrive")
}

func TestVendorProgressFailureUsesFreshContextAndRedacts(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			service, _, _, fake, _ := testService(t)
			cfg := service.vendorBaseConfig()
			cfg.Proxy = "socks5h://username:proxy-password@host:1080"
			cfg.Translation.APIKey = "translate-secret"
			q := &vendorProgressQQ{fakeQQ: fake}
			service.qq = q
			service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
				if timeout {
					<-ctx.Done()
					return vendorstatus.Packet{}, ctx.Err()
				}
				callbacks.Progress(vendorstatus.Packet{Stage: "warning", Text: "HTTP 502 socks5h://username:proxy-password@host:1080 translate-secret"})
				return vendorstatus.Packet{}, errors.New("ImportError: proxy-password translate-secret")
			}}
			deadline := 3 * time.Second
			if timeout {
				deadline = 20 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			err := service.runVendorQueryWithProgress(ctx, groupEvent("g", "u", "/vendor_status"), cfg, 5*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			text := strings.Join(q.texts, "\n")
			if !strings.Contains(text, "查询失败") || strings.Contains(text, "proxy-password") || strings.Contains(text, "translate-secret") {
				t.Fatal(text)
			}
			if timeout && !strings.Contains(text, "查询超时") {
				t.Fatal(text)
			}
			if !timeout && !strings.Contains(text, "ImportError") {
				t.Fatal("reason hidden", text)
			}
			if len(q.recalled) == 0 {
				t.Fatal("failure left progress behind")
			}
		})
	}
}

func TestVendorProgressPartialWarningDoesNotPreventImage(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake}
	service.qq = q
	service.vendorRunner = &fakeVendorRunner{run: func(_ context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		callbacks.Progress(vendorstatus.Packet{Stage: "warning", Text: "OpenAI: TimeoutError"})
		return vendorstatus.Packet{PNG: []byte("png")}, nil
	}}
	if err := service.runVendorQueryWithProgress(context.Background(), groupEvent("g", "u", "/vendor_status"), service.vendorBaseConfig(), time.Second); err != nil {
		t.Fatal(err)
	}
	warningIndex := -1
	for i, text := range q.texts {
		if strings.Contains(text, "TimeoutError") {
			warningIndex = i
			break
		}
	}
	if warningIndex < 0 {
		t.Fatal("missing warning", q.texts)
	}
	warningID := fmt.Sprintf("progress-%d", warningIndex+1)
	for _, id := range q.recalled {
		if id == warningID {
			t.Fatal("warning was retracted", q.recalled)
		}
	}
	if len(q.recalled) != len(q.texts)-1 {
		t.Fatal("progress was not cleaned", q.recalled, q.texts)
	}
}

func TestVendorProgressPublishesEveryStepImmediatelyAndInOrder(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake}
	service.qq = q
	steps := []vendorstatus.Packet{
		{Stage: "fetch", Text: "正在收集 OpenAI 信息……", Completed: 0, Total: 2},
		{Stage: "fetch", Text: "正在收集 Claude 信息……", Completed: 1, Total: 2},
		{Stage: "translate", Text: "正在翻译事件信息……"},
		{Stage: "render", Text: "正在生成状态总览图片……"},
	}
	service.vendorRunner = &fakeVendorRunner{run: func(_ context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		for _, step := range steps {
			callbacks.Progress(step)
		}
		// Repeated reports of the same step do not create duplicate messages.
		callbacks.Progress(steps[len(steps)-1])
		return vendorstatus.Packet{PNG: []byte("png")}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := service.runVendorQueryWithProgress(ctx, groupEvent("g", "u", "/vendor_status"), service.vendorBaseConfig(), time.Hour); err != nil {
		t.Fatal(err)
	}
	position := 0
	for _, step := range steps {
		matches := 0
		for i, message := range q.texts {
			if strings.HasPrefix(message, step.Text) {
				matches++
				if i < position {
					t.Fatal("step order changed", q.texts)
				}
				position = i + 1
			}
		}
		if matches != 1 {
			t.Fatal("step missing or duplicated before idle interval", step.Text, q.texts)
		}
	}
	if !strings.Contains(q.texts[len(q.texts)-1], "正在发送状态总览图片") {
		t.Fatal(q.texts)
	}
	if len(q.recalled) != len(q.texts) {
		t.Fatal("progress cleanup incomplete", q.recalled)
	}
}

func TestVendorProgressHeartbeatRefreshesCurrentStepAndResetsOnTransition(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake}
	service.qq = q
	next := make(chan struct{})
	finish := make(chan struct{})
	const first = "正在收集 OpenAI 信息……"
	const second = "正在生成状态总览图片……"
	service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		callbacks.Progress(vendorstatus.Packet{Stage: "fetch", Text: first})
		select {
		case <-next:
		case <-ctx.Done():
			return vendorstatus.Packet{}, ctx.Err()
		}
		callbacks.Progress(vendorstatus.Packet{Stage: "render", Text: second})
		select {
		case <-finish:
		case <-ctx.Done():
			return vendorstatus.Packet{}, ctx.Err()
		}
		return vendorstatus.Packet{PNG: []byte("png")}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	interval := 80 * time.Millisecond
	go func() {
		done <- service.runVendorQueryWithProgress(ctx, groupEvent("g", "u", "/vendor_status"), service.vendorBaseConfig(), interval)
	}()
	awaitVendorProgress(t, func() bool {
		q.historyMu.Lock()
		defer q.historyMu.Unlock()
		for _, text := range q.texts {
			if text == first {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond)
	close(next)
	awaitVendorProgress(t, func() bool {
		q.historyMu.Lock()
		defer q.historyMu.Unlock()
		count := 0
		for _, text := range q.texts {
			if text == second {
				count++
			}
		}
		return count >= 2
	})
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var times []time.Time
	for i, text := range q.texts {
		if text == second {
			times = append(times, q.textTimes[i])
		}
	}
	if times[1].Sub(times[0]) < interval-5*time.Millisecond {
		t.Fatal("idle timer was not reset by new step", times[1].Sub(times[0]))
	}
	if len(q.recalled) != len(q.texts) {
		t.Fatal("heartbeat left previous progress", q.history)
	}
}

func TestVendorProgressDoesNotRecallPreviousWhenNewSendFails(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake, textErrAt: 2}
	service.qq = q
	replies := &vendorQueryReplies{s: service, event: groupEvent("g", "u", "/vendor_status"), started: time.Now()}
	replies.update(context.Background(), "first")
	replies.update(context.Background(), "failed")
	if len(q.recalled) != 0 {
		t.Fatal("old progress lost", q.recalled)
	}
	replies.update(context.Background(), "third")
	if len(q.recalled) != 1 || q.recalled[0] != "progress-1" {
		t.Fatal(q.recalled)
	}
}

func TestVendorProgressRecallsImmediatePreviousBeforeOlderFailures(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake, recallErr: true}
	service.qq = q
	replies := &vendorQueryReplies{s: service, event: groupEvent("g", "u", "/vendor_status"), started: time.Now()}
	replies.update(context.Background(), "first")
	replies.update(context.Background(), "second")
	q.recallErr = false
	start := len(q.history)
	replies.update(context.Background(), "third")
	if len(q.history) < start+3 || q.history[start] != "text:progress-3" || q.history[start+1] != "recall:progress-2" || q.history[start+2] != "recall:progress-1" {
		t.Fatal("old failure delayed immediate previous progress recall", q.history)
	}
}

func TestVendorRepeatedSameStepDoesNotSuppressIdleRefresh(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake}
	service.qq = q
	finish := make(chan struct{})
	service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		ticks := time.NewTicker(5 * time.Millisecond)
		defer ticks.Stop()
		for {
			callbacks.Progress(vendorstatus.Packet{Stage: "render", Text: "正在生成状态总览图片……"})
			select {
			case <-ctx.Done():
				return vendorstatus.Packet{}, ctx.Err()
			case <-finish:
				return vendorstatus.Packet{PNG: []byte("png")}, nil
			case <-ticks.C:
			}
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.runVendorQueryWithProgress(ctx, groupEvent("g", "u", "/vendor_status"), service.vendorBaseConfig(), 20*time.Millisecond)
	}()
	awaitVendorProgress(t, func() bool {
		q.historyMu.Lock()
		defer q.historyMu.Unlock()
		count := 0
		for _, message := range q.texts {
			if message == "正在生成状态总览图片……" {
				count++
			}
		}
		return count >= 3
	})
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestVendorReplySequencesAreUniqueAndExpireToIndependentMessages(t *testing.T) {
	service, _, _, _, _ := testService(t)
	replies := &vendorQueryReplies{s: service, event: groupEvent("g", "u", "/vendor_status"), started: time.Now()}
	for want := 1; want <= 4; want++ {
		event, seq := replies.reserve()
		if event.Message.ID == "" || seq != want {
			t.Fatal(event.Message.ID, seq)
		}
	}
	event, seq := replies.reserve()
	if event.Message.ID != "" || seq != 1 {
		t.Fatal("reply budget not respected")
	}
	for range 20 {
		replies.reserve()
	}
	event, seq = replies.reserveResult()
	if event.Message.ID == "" || seq != 5 {
		t.Fatal("progress exhausted the final image reply budget", event.Message.ID, seq)
	}
	event, _ = replies.reserveResult()
	if event.Message.ID != "" {
		t.Fatal("final sequence reused")
	}
	replies.nextSeq = 0
	replies.started = time.Now().Add(-3 * time.Minute)
	event, _ = replies.reserve()
	if event.Message.ID != "" {
		t.Fatal("expired reply ID reused")
	}
}

func TestVendorProgressOutlivesParentDeadlineAndReportsImageFailure(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake, imageErr: errors.New("QQ upload HTTP 403 token=private-value")}
	service.qq = q
	service.vendorRunner = &fakeVendorRunner{run: func(ctx context.Context, _ vendorstatus.Request, _ vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		if ctx.Err() != nil {
			t.Error("short parent deadline canceled generation")
		}
		return vendorstatus.Packet{PNG: []byte("png")}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.handleVendorStatusWithProgress(ctx, groupEvent("g", "u", "/vendor_status"), []string{"/vendor_status"}, time.Second); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(q.texts, "\n")
	if !strings.Contains(text, "HTTP 403") || strings.Contains(text, "private-value") || !strings.Contains(text, "查询失败") {
		t.Fatal(text)
	}
}

func TestVendorProgressWhileWaitingForExecutionSlot(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake}
	service.qq = q
	service.vendorSemaphore <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := service.runVendorQueryWithProgress(ctx, groupEvent("g", "u", "/vendor_status"), service.vendorBaseConfig(), 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	<-service.vendorSemaphore
	if len(q.texts) < 2 || !strings.Contains(q.texts[0], "等待执行槽位") {
		t.Fatal(q.texts)
	}
}

func TestVendorProgressRedactsWorkerSnapshotAfterConfigurationChanges(t *testing.T) {
	service, _, _, fake, _ := testService(t)
	q := &vendorProgressQQ{fakeQQ: fake}
	service.qq = q
	initial := service.vendorBaseConfig()
	if err := service.setVendorConfigValue("proxy", "socks5h://old-user:old-password@host:1080", false); err != nil {
		t.Fatal(err)
	}
	service.vendorRunner = &fakeVendorRunner{run: func(_ context.Context, _ vendorstatus.Request, callbacks vendorstatus.Callbacks) (vendorstatus.Packet, error) {
		if err := service.setVendorConfigValue("proxy", "socks5h://new-user:new-password@host:1080", false); err != nil {
			return vendorstatus.Packet{}, err
		}
		callbacks.Progress(vendorstatus.Packet{Stage: "warning", Text: "ProxyError: old-user old-password"})
		return vendorstatus.Packet{}, errors.New("ProxyError: old-password")
	}}
	if err := service.runVendorQueryWithProgress(context.Background(), groupEvent("g", "u", "/vendor_status"), initial, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, message := range q.texts {
		if strings.Contains(message, "old-password") || strings.Contains(message, "old-user") {
			t.Fatal("previous worker credentials leaked", message)
		}
	}
}

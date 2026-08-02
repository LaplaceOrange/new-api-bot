package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fsykk/new-api-bot/internal/config"
	"github.com/fsykk/new-api-bot/internal/model"
	"github.com/fsykk/new-api-bot/internal/newapi"
	"github.com/fsykk/new-api-bot/internal/store"
)

func runMaintenance(cfg config.Config, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "maintenance-bind" {
		return false, nil
	}
	if len(args) != 3 && len(args) != 4 {
		return true, errors.New("用法：maintenance-bind <QQ OpenID标识> <New API用户ID> [--skip-user-check]")
	}
	skipUserCheck := len(args) == 4 && args[3] == "--skip-user-check"
	if len(args) == 4 && !skipUserCheck {
		return true, errors.New("未知维护参数，仅支持 --skip-user-check")
	}
	canonical := strings.TrimSpace(args[1])
	if !validCanonicalIdentity(canonical) {
		return true, errors.New("QQ OpenID 标识格式无效，必须使用 union:、user: 或 member:<group_openid>:<member_openid>")
	}
	userID, err := strconv.Atoi(args[2])
	if err != nil || userID <= 0 {
		return true, errors.New("New API 用户 ID 必须是正整数")
	}

	user := newapi.User{ID: userID}
	if !skipUserCheck {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.NewAPITimeout)
		defer cancel()
		client := newapi.New(cfg.NewAPIBaseURL, cfg.NewAPIAdminToken, cfg.NewAPIAdminUserID, cfg.NewAPITimeout)
		user, err = client.GetUser(ctx, userID)
		if err != nil {
			return true, fmt.Errorf("查询 New API 用户 %d 失败: %w", userID, err)
		}
		if user.Status != 1 {
			return true, fmt.Errorf("New API 用户 %d 当前未启用", userID)
		}
		if strings.TrimSpace(user.Email) == "" {
			return true, fmt.Errorf("New API 用户 %d 没有绑定邮箱", userID)
		}
	}

	storage, err := store.Open(cfg.DataPath)
	if err != nil {
		return true, fmt.Errorf("打开数据库失败: %w", err)
	}
	defer storage.Close()
	if existing, err := storage.GetBinding(canonical); err == nil {
		if existing.NewAPIID == user.ID {
			fmt.Printf("绑定已经存在：%s -> New API 用户 %d\n", canonical, user.ID)
			return true, nil
		}
		return true, fmt.Errorf("该 QQ OpenID 已绑定 New API 用户 %d，未改写现有绑定", existing.NewAPIID)
	}
	if existing, err := storage.GetBindingByNewAPIID(user.ID); err == nil {
		return true, fmt.Errorf("New API 用户 %d 已绑定其他 QQ OpenID：%s", user.ID, existing.CanonicalID)
	}
	now := time.Now()
	binding := model.Binding{
		CanonicalID: canonical,
		NewAPIID:    user.ID,
		Email:       strings.ToLower(strings.TrimSpace(user.Email)),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := storage.CreateBinding(binding); err != nil {
		return true, fmt.Errorf("创建绑定失败: %w", err)
	}
	_ = storage.AddAudit(model.AuditRecord{
		At:      now,
		Actor:   "maintenance:manual",
		Action:  "binding.create",
		Target:  strconv.Itoa(user.ID),
		Success: true,
		Metadata: map[string]any{
			"canonical_id": canonical,
			"user_checked": !skipUserCheck,
		},
	})
	fmt.Printf("绑定成功：%s -> New API 用户 %d\n", canonical, user.ID)
	return true, nil
}

func validCanonicalIdentity(value string) bool {
	if strings.HasPrefix(value, "union:") {
		return len(strings.TrimPrefix(value, "union:")) > 0
	}
	if strings.HasPrefix(value, "user:") {
		return len(strings.TrimPrefix(value, "user:")) > 0
	}
	if strings.HasPrefix(value, "member:") {
		parts := strings.Split(value, ":")
		return len(parts) == 3 && parts[1] != "" && parts[2] != ""
	}
	return false
}

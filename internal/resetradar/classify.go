package resetradar

import (
	"html"
	"regexp"
	"strings"
)

var (
	spacePattern     = regexp.MustCompile(`\s+`)
	productPattern   = regexp.MustCompile(`(?i)\b(?:codex|chatgpt[ -]?work|workspace agent)\b`)
	quotaPattern     = regexp.MustCompile(`(?i)\b(?:(?:usage|rate|weekly|five[ -]?hour|5[ -]?hour)[ -]?limits?|quotas?|credits?|resets?|fresh\s+(?:codex\s+)?limits?)\b|额度|配额|限额|重置`)
	negativePattern  = regexp.MustCompile(`(?i)\b(?:no|not|never|won't|wouldn't|isn't|aren't|can't|cannot|without)\b.{0,45}\b(?:reset|resets|resetting)\b|\bno\s+(?:(?:codex|usage|limit)\s+)?resets?\b|(?:不会|没有|并未|不再|无法).{0,20}重置`)
	completedPattern = regexp.MustCompile(`(?i)\b(?:we|openai|team|i)\b\s+(?:have|has|had)\s+(?:(?:already|just|now|successfully|fully)\s+)*reset\b|\b(?:we|openai|team|i)\b\s+(?:(?:already|just|now|successfully|fully)\s+)+reset\b|\bi['’]ve\s+(?:(?:already|just|now|successfully|fully)\s+)*reset\b|\b(?:(?:usage|rate|weekly|five[ -]?hour|5[ -]?hour)[ -]?limits?|quotas?|credits?)\b.{0,30}\b(?:have|has|had)\s+(?:(?:already|just|now|successfully|fully)\s+)*been\s+(?:(?:successfully|fully)\s+)*reset\b|\b(?:(?:usage|rate|weekly|five[ -]?hour|5[ -]?hour)[ -]?limits?|quotas?|credits?)\b.{0,30}\b(?:were|was|are now|is now|already|just|now)\s+(?:(?:successfully|fully)\s+)*reset\b|\breset\b.{0,25}\b(?:is|was|went|has gone)\s+(?:now\s+)?(?:complete|completed|done|live)\b|\breset\s+(?:is\s+)?(?:complete|completed|done|live)\b|(?:额度|配额|限额)(?:已经|已|现已).{0,8}重置|重置(?:已经|已|现已)?(?:完成|完成了|生效|上线)`)
	imminentPattern  = regexp.MustCompile(`(?i)\b(?:reset|resets|resetting)\b.{0,100}\b(?:coming|soon|imminent|today|tonight|tomorrow|later|this week|next\s+(?:hour|few hours|day|few days|week|month|monday|tuesday|wednesday|thursday|friday|saturday|sunday)|few hours|around the corner|on the way)\b|\b(?:will|shall|going to|gonna|i'll|we'll|promised|may|might|could|should|probably|expected|predicted|likely|scheduled|planned|set|due|plan|plans|intend|intends|intended|aim|aims|prepare|prepares|preparing|decide|decides|decided|hope|hopes|hoping)\b.{0,65}\breset\b|\b(?:expect|expects|expecting|predict|predicts|predicting)\b.{0,65}\breset\b|\b(?:about|ready)\s+to\b.{0,35}\breset\b|\b(?:reset|resets?)\b.{0,50}\b(?:is|are|has been|have been)\s+(?:now\s+|currently\s+)?(?:expected|predicted|likely|planned|scheduled|set|due)\b|\blands?\s+in\s+(?:the\s+)?next\s+(?:hour|few hours)\b|\bin\s+(?:a\s+)?few\s+hours\b.{0,70}\b(?:reset|laptop)\b|\bfeeling like a (?:limit|usage) reset\b|(?:即将|将(?:会|在)?|预计|预期|可能|或将|计划|准备).{0,20}重置|重置.{0,20}(?:即将|预计|预期|稍后|今天|今晚|明天)`)
	confirmedPattern = regexp.MustCompile(`(?i)\b(?:we|openai|team|i|limits?|usage)\b.{0,55}\b(?:have|has|were|was|just|now|i've)\b.{0,30}\breset\b|\b(?:reset|resetting)\b.{0,45}\b(?:(?:usage|rate|weekly)[ -]?)?limits?\b|\blimits?\b.{0,45}\b(?:have been|has been|were|are now)\s+reset\b|\breset\s+(?:is\s+)?(?:complete|completed|done|live)\b|\bfresh\s+(?:codex\s+)?limits?\b`)
	resetTermPattern = regexp.MustCompile(`(?i)\b(?:reset|resets|resetting|quota|usage limit|rate limit|weekly limit|fresh limits?)\b|重置|额度|配额`)
	hintTimePattern  = regexp.MustCompile(`(?i)\b(?:today|tonight|tomorrow|monday|soon|later)\b`)
)

// Classify returns the confidence stage for one public signal. The trusted
// Tibo timeline may supply the product context, but no source bypasses quota
// wording or negation checks.
func Classify(text, kind string, score int) Stage {
	normalized := normalizeText(text)
	trustedTiboPost := strings.EqualFold(kind, "x-tibo")
	if normalized == "" || !quotaPattern.MatchString(normalized) || (!trustedTiboPost && !productPattern.MatchString(normalized)) {
		return StageUnknown
	}
	if negativePattern.MatchString(normalized) {
		return StageUnknown
	}
	// A completed reset is stronger evidence than future wording elsewhere in
	// the same post (for example, "limits have been reset and are now live").
	if completedPattern.MatchString(normalized) {
		return StageConfirmed
	}
	// Check forecasts before the broader in-progress confirmation patterns. A
	// phrase such as "usage reset is expected tomorrow" must not start an event.
	if imminentPattern.MatchString(normalized) ||
		(strings.EqualFold(kind, "hint") && score >= 90 && hintTimePattern.MatchString(normalized)) {
		return StageImminent
	}
	if confirmedPattern.MatchString(normalized) {
		return StageConfirmed
	}
	if resetTermPattern.MatchString(normalized) &&
		(strings.HasPrefix(strings.ToLower(kind), "x-") || strings.EqualFold(kind, "hint") || (!strings.EqualFold(kind, "official-status") && score >= 80)) {
		return StagePossible
	}
	return StageUnknown
}

func normalizeText(value string) string {
	value = html.UnescapeString(value)
	value = strings.TrimSpace(spacePattern.ReplaceAllString(value, " "))
	return value
}

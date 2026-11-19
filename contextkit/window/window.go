package window

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/eruca/goagents/contextkit"
	"github.com/eruca/goagents/contextkit/toolprojection"
)

const (
	kindKey             = "contextkit.kind"
	collapseIDKey       = "contextkit.collapse_id"
	droppedTotalKey     = "contextkit.dropped_total"
	droppedImagesKey    = "contextkit.dropped_images"
	kindWindowSummary   = "window_summary"
	kindCollapseSummary = "collapse_summary"

	placeholderPrefix = "compressed earlier context: "
	deepSummaryPrefix = "summary of earlier context ("
	// maxListedIDs bounds the dropped message IDs a placeholder names, so
	// the placeholder does not grow with the number of dropped messages.
	maxListedIDs = 5
)

var (
	omittedPattern = regexp.MustCompile(`(\d+) messages omitted`)
	imagesPattern  = regexp.MustCompile(`(\d+) images`)
)

// ErrTokenCounterRequired reports a MaxTokens budget without CountTokens:
// guessing tokens from characters could overrun the model's window.
var ErrTokenCounterRequired = errors.New("contextkit/window: Budget.MaxTokens needs Config.CountTokens")

type Config struct {
	Budget contextkit.Budget
	// Profile defaults to contextkit.ProfileStandard. Pass
	// contextkit.ProfileFromEnv() to follow CONTEXT_DEEP_COMPRESSION.
	Profile contextkit.Profile
	// MinRecent is the number of trailing messages always kept, even over
	// budget; at least 1. The kept window then extends back to the start of
	// its turn.
	MinRecent    int
	MaxToolChars int
	// CountTokens prices messages for Budget.MaxTokens and is called only when
	// that limit is set. It sees tool messages after projection and must count
	// a message's images.
	CountTokens contextkit.TokenCounter
	Summarizer  contextkit.Summarizer
	// SummaryReserve is the part of the budget kept for the summary: the
	// window fills the rest, and a Summarizer's summary is truncated to fit
	// it. With a Summarizer and no reserve, a fifth of each budget limit is
	// reserved. Without a Summarizer and no reserve, the window leaves room
	// for the placeholder summary it will write.
	SummaryReserve contextkit.Budget
}

// Compressor keeps the newest whole turns that fit the budget and replaces
// everything older with one summary message. The budget covers system
// messages, the summary, and the kept conversation, in characters, tokens, or
// both.
//
// Compress is incremental: feed it its previous output plus the new messages
// and only the newly dropped messages are summarized, together with the
// previous summary, instead of the whole history again.
type Compressor struct {
	cfg Config
}

func New(cfg Config) *Compressor {
	if cfg.Profile == "" {
		cfg.Profile = contextkit.ProfileStandard
	}
	if cfg.MinRecent <= 0 {
		cfg.MinRecent = 1
	}
	return &Compressor{cfg: cfg}
}

func (c *Compressor) Compress(ctx context.Context, req contextkit.Request) (*contextkit.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	profile := c.cfg.Profile
	if profile == "" {
		profile = contextkit.ProfileStandard
	}
	budget := effectiveBudget(req.Budget, c.cfg.Budget)
	if budget.MaxTokens > 0 && c.cfg.CountTokens == nil {
		return nil, ErrTokenCounterRequired
	}

	messages := budgetToolMessages(req.Messages, c.cfg.MaxToolChars)
	system, previous, conversation := split(messages)
	costOf := c.coster(budget)
	base := costOf(system)
	reserve := c.summaryReserve(budget)
	// The window leaves room for the summary: a Summarizer's reserve, or the
	// placeholder the dropped messages would get.
	fits := func(dropped []contextkit.Message, kept cost) bool {
		withSummary := base.add(kept)
		if reserve.Bounded() {
			return budget.minus(reserve).fits(withSummary)
		}
		if len(dropped) > 0 || len(previous) > 0 {
			withSummary = withSummary.add(costOf([]contextkit.Message{c.placeholder(previous, dropped)}))
		}
		return budget.fits(withSummary)
	}
	// Keeping the whole conversation needs no new summary, so it can fit
	// where a shorter window plus a summary does not.
	dropped, recent, kept := []contextkit.Message(nil), conversation, costOf(conversation)
	if budget.Bounded() && !budget.fits(base.add(costOf(previous)).add(kept)) {
		dropped, recent, kept = windowStart(conversation, c.cfg.MinRecent, costOf, fits)
	}

	result := &contextkit.Result{
		Profile: profile,
		Levels:  contextkit.LevelsForProfile(profile),
		Dropped: len(dropped),
	}
	out := append([]contextkit.Message(nil), system...)
	var summaries []contextkit.Message
	switch {
	case len(dropped) > 0:
		summary, err := c.summarize(ctx, profile, previous, dropped, recent, budget, reserve, req.Metadata, result)
		if err != nil {
			return nil, err
		}
		summaries = []contextkit.Message{summary}
		result.Summary = summary.Content
	case len(previous) > 0:
		summaries = previous
		result.Summary = previous[len(previous)-1].Content
	}
	out = append(out, summaries...)
	out = append(out, recent...)

	result.Messages = out
	result.OverBudget = budget.Bounded() && !budget.fits(base.add(costOf(summaries)).add(kept))
	return result, nil
}

// summaryReserve is the configured reserve, or a fifth of each limit when a
// Summarizer writes the summary.
func (c *Compressor) summaryReserve(budget limits) limits {
	if c.cfg.SummaryReserve.Bounded() {
		return limits{Budget: c.cfg.SummaryReserve}
	}
	if c.cfg.Summarizer == nil || c.cfg.Profile != contextkit.ProfileDeep || !budget.Bounded() {
		return limits{}
	}
	fifth := func(limit int) int {
		if limit <= 0 {
			return 0
		}
		return max(limit/5, 1)
	}
	return limits{Budget: contextkit.Budget{MaxChars: fifth(budget.MaxChars), MaxTokens: fifth(budget.MaxTokens)}}
}

// placeholder is the summary written without a Summarizer for the dropped
// messages, counting those the previous summaries already omitted.
func (c *Compressor) placeholder(previous, dropped []contextkit.Message) contextkit.Message {
	total, images := len(dropped), imageCount(dropped)
	for _, msg := range previous {
		total += summaryCount(msg, droppedTotalKey, omittedPattern)
		images += summaryCount(msg, droppedImagesKey, imagesPattern)
	}
	summary := contextkit.Message{
		Role:     contextkit.RoleSystem,
		Content:  placeholderSummary(total, images, dropped),
		Metadata: map[string]any{kindKey: kindWindowSummary, droppedTotalKey: total},
	}
	if images > 0 {
		summary.Metadata[droppedImagesKey] = images
	}
	return summary
}

// truncateToFit shortens a summary's content until it fits the reserve.
func (c *Compressor) truncateToFit(msg contextkit.Message, reserve limits, costOf func([]contextkit.Message) cost) contextkit.Message {
	if !reserve.Bounded() || reserve.fits(costOf([]contextkit.Message{msg})) {
		return msg
	}
	runes := []rune(msg.Content)
	fitsAt := func(n int) bool {
		candidate := msg
		candidate.Content = string(runes[:n])
		return reserve.fits(costOf([]contextkit.Message{candidate}))
	}
	// The longest prefix that fits; cost grows with length.
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if fitsAt(mid) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	msg.Content = string(runes[:low])
	return msg
}

// summarize builds the summary message that replaces the previous summary and
// the newly dropped messages. Deep mode also records a reversible collapse.
func (c *Compressor) summarize(ctx context.Context, profile contextkit.Profile, previous, dropped, recent []contextkit.Message, budget, reserve limits, metadata map[string]any, result *contextkit.Result) (contextkit.Message, error) {
	summary := c.placeholder(previous, dropped)
	if profile != contextkit.ProfileDeep {
		return c.truncateToFit(summary, reserve, c.coster(budget)), nil
	}

	if c.cfg.Summarizer != nil {
		compacted, err := c.cfg.Summarizer.Summarize(ctx, contextkit.SummarizeRequest{
			Previous:      joinContents(previous),
			Collapsed:     cloneMessages(dropped),
			Recent:        cloneMessages(recent),
			Budget:        budget.Budget,
			SummaryBudget: reserve.Budget,
			Metadata:      cloneMetadata(metadata),
		})
		if err != nil {
			return contextkit.Message{}, err
		}
		if compacted != nil && compacted.Summary != "" {
			total := summary.Metadata[droppedTotalKey].(int)
			header := fmt.Sprintf("%s%d messages omitted", deepSummaryPrefix, total)
			if images, _ := summary.Metadata[droppedImagesKey].(int); images > 0 {
				header += fmt.Sprintf(", %d images", images)
			}
			header += "):\n"
			summary.Content = header + compacted.Summary
			summary = c.truncateToFit(summary, reserve, c.coster(budget))
			result.AutoCompact = &contextkit.Compact{
				Summary:  strings.TrimPrefix(summary.Content, header),
				Metadata: cloneMetadata(compacted.Metadata),
			}
		}
	}
	// The previous summary is an original too, so restoring this collapse
	// yields it back and an earlier collapse can then expand it in turn.
	collapse := buildCollapse(append(cloneMessages(previous), dropped...), summary)
	summary.Metadata[kindKey] = kindCollapseSummary
	summary.Metadata[collapseIDKey] = collapse.ID
	collapse.Summary = cloneMessage(summary)
	result.Collapses = []contextkit.Collapse{collapse}
	return summary, nil
}

// cost is what messages take from a Budget.
type cost struct {
	chars, tokens int
}

func (c cost) add(o cost) cost {
	return cost{chars: c.chars + o.chars, tokens: c.tokens + o.tokens}
}

// limits adds the fit check to contextkit.Budget. A limit the budget sets
// stays set after minus, even when nothing of it is left.
type limits struct {
	contextkit.Budget
	exhaustedChars, exhaustedTokens bool
}

func (b limits) fits(c cost) bool {
	return !b.exhaustedChars && !b.exhaustedTokens &&
		(b.MaxChars <= 0 || c.chars <= b.MaxChars) && (b.MaxTokens <= 0 || c.tokens <= b.MaxTokens)
}

// minus is what is left of b after the reserve.
func (b limits) minus(reserve limits) limits {
	left := b
	if b.MaxChars > 0 && reserve.MaxChars > 0 {
		left.MaxChars = b.MaxChars - reserve.MaxChars
		left.exhaustedChars = left.MaxChars <= 0
	}
	if b.MaxTokens > 0 && reserve.MaxTokens > 0 {
		left.MaxTokens = b.MaxTokens - reserve.MaxTokens
		left.exhaustedTokens = left.MaxTokens <= 0
	}
	return left
}

// coster prices messages for the limits the budget sets, so CountTokens runs
// only under a token limit.
func (c *Compressor) coster(b limits) func([]contextkit.Message) cost {
	return func(messages []contextkit.Message) cost {
		var total cost
		for _, msg := range messages {
			total.chars += msg.CharLen()
			if b.MaxTokens > 0 {
				total.tokens += c.cfg.CountTokens(msg)
			}
		}
		return total
	}
}

// windowStart splits a conversation that does not fit whole into the
// dropped messages and the kept window, and returns what the window costs.
// The window always holds the last minRecent messages and starts on a step
// boundary (see stepBoundaries), so no tool reply loses its call. It extends
// back over earlier steps while they fit together with the summary of the
// rest. When it starts inside the last turn, the turn's user message is kept
// too, ahead of the window, so the model keeps the request it is working on.
func windowStart(messages []contextkit.Message, minRecent int, costOf func([]contextkit.Message) cost, fits func([]contextkit.Message, cost) bool) (dropped, kept []contextkit.Message, keptCost cost) {
	boundaries, lastUser := stepBoundaries(messages)
	split := func(cut int) ([]contextkit.Message, []contextkit.Message) {
		if lastUser >= 0 && cut > lastUser {
			dropped := append(append([]contextkit.Message(nil), messages[:lastUser]...), messages[lastUser+1:cut]...)
			return dropped, append([]contextkit.Message{messages[lastUser]}, messages[cut:]...)
		}
		return messages[:cut], messages[cut:]
	}
	limit := max(len(messages)-minRecent, 0)
	cut := 0
	for _, b := range boundaries {
		if b <= limit {
			cut = b
		}
	}
	dropped, kept = split(cut)
	keptCost = costOf(kept)
	for i := len(boundaries) - 1; i >= 0; i-- {
		b := boundaries[i]
		if b >= cut {
			continue
		}
		nextDropped, nextKept := split(b)
		nextCost := costOf(nextKept)
		if !fits(nextDropped, nextCost) {
			break
		}
		cut, dropped, kept, keptCost = b, nextDropped, nextKept, nextCost
	}
	return dropped, kept, keptCost
}

// stepBoundaries lists the indexes a kept window may start at, and the index
// of the last user message (-1 without one). Each user message starts a turn.
// Within the last turn, which an agent run extends with tool calls, each
// assistant message also starts a step: it is followed by the replies to its
// tool calls. Without user messages every non-tool message is a boundary.
// Index 0 is always a boundary.
func stepBoundaries(messages []contextkit.Message) ([]int, int) {
	lastUser := -1
	for i, msg := range messages {
		if msg.Role == contextkit.RoleUser {
			lastUser = i
		}
	}
	boundaries := []int{0}
	for i := 1; i < len(messages); i++ {
		role := messages[i].Role
		switch {
		case role == contextkit.RoleUser:
			boundaries = append(boundaries, i)
		case lastUser < 0 && role != contextkit.RoleTool:
			boundaries = append(boundaries, i)
		case lastUser >= 0 && i > lastUser+1 && role == contextkit.RoleAssistant:
			boundaries = append(boundaries, i)
		}
	}
	return boundaries, lastUser
}

func budgetToolMessages(messages []contextkit.Message, maxToolChars int) []contextkit.Message {
	out := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		copied := cloneMessage(msg)
		if copied.Role == contextkit.RoleTool {
			copied = toolprojection.Project(copied, toolprojection.Config{MaxResultChars: maxToolChars})
		}
		out = append(out, copied)
	}
	return out
}

// split separates real system messages and summaries left by an earlier
// Compress from the conversation.
func split(messages []contextkit.Message) (system, previous, conversation []contextkit.Message) {
	for _, msg := range messages {
		switch {
		case isSummary(msg):
			previous = append(previous, msg)
		case msg.Role == contextkit.RoleSystem:
			system = append(system, msg)
		default:
			conversation = append(conversation, msg)
		}
	}
	return system, previous, conversation
}

// isSummary recognizes a summary an earlier Compress wrote: by its metadata,
// or, when a host stored it without metadata, by the header its content
// starts with.
func isSummary(msg contextkit.Message) bool {
	kind, _ := msg.Metadata[kindKey].(string)
	if kind == kindWindowSummary || kind == kindCollapseSummary {
		return true
	}
	return msg.Role == contextkit.RoleSystem &&
		(strings.HasPrefix(msg.Content, placeholderPrefix) || strings.HasPrefix(msg.Content, deepSummaryPrefix))
}

// summaryCount reads a count a summary carries: from its metadata, also after
// a JSON round trip has turned it into a float64, or else from its header.
func summaryCount(msg contextkit.Message, key string, pattern *regexp.Regexp) int {
	switch n := msg.Metadata[key].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	header, _, _ := strings.Cut(msg.Content, "\n")
	if match := pattern.FindStringSubmatch(header); match != nil {
		if n, err := strconv.Atoi(match[1]); err == nil {
			return n
		}
	}
	return 0
}

func placeholderSummary(total, images int, dropped []contextkit.Message) string {
	text := fmt.Sprintf("compressed earlier context: %d messages omitted", total)
	if images > 0 {
		text += fmt.Sprintf(" (%d images)", images)
	}
	ids := make([]string, 0, len(dropped))
	for _, msg := range dropped {
		if msg.ID != "" {
			ids = append(ids, msg.ID)
		}
	}
	if len(ids) > maxListedIDs {
		ids = ids[len(ids)-maxListedIDs:]
	}
	if len(ids) > 0 {
		text += ", latest ids: " + strings.Join(ids, ", ")
	}
	return text
}

func imageCount(messages []contextkit.Message) int {
	total := 0
	for _, msg := range messages {
		total += len(msg.Images)
	}
	return total
}

func joinContents(messages []contextkit.Message) string {
	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		contents = append(contents, msg.Content)
	}
	return strings.Join(contents, "\n")
}

func buildCollapse(messages []contextkit.Message, summary contextkit.Message) contextkit.Collapse {
	ids := make([]string, 0, len(messages))
	for i, msg := range messages {
		switch {
		case msg.ID != "":
			ids = append(ids, msg.ID)
		case isSummary(msg):
			id, _ := msg.Metadata[collapseIDKey].(string)
			ids = append(ids, "summary:"+id)
		default:
			ids = append(ids, fmt.Sprintf("index:%d", i))
		}
	}
	return contextkit.Collapse{
		ID:          collapseID(ids),
		StartIndex:  0,
		EndIndex:    len(messages) - 1,
		Summary:     summary,
		OriginalIDs: ids,
		Originals:   cloneMessages(messages),
	}
}

func collapseID(ids []string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(strings.Join(ids, "\x00")))
	return "collapse-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// effectiveBudget prefers a request budget that sets any limit, taken whole:
// a limit it leaves unset does not fall back to the config.
func effectiveBudget(requestBudget, configBudget contextkit.Budget) limits {
	if requestBudget.Bounded() {
		return limits{Budget: requestBudget}
	}
	return limits{Budget: configBudget}
}

func cloneMessage(msg contextkit.Message) contextkit.Message {
	if len(msg.ToolCalls) > 0 {
		msg.ToolCalls = append([]contextkit.ToolCall(nil), msg.ToolCalls...)
	} else {
		msg.ToolCalls = nil
	}
	if len(msg.Images) > 0 {
		msg.Images = append([]contextkit.Image(nil), msg.Images...)
	} else {
		msg.Images = nil
	}
	msg.Metadata = cloneMetadata(msg.Metadata)
	return msg
}

func cloneMessages(messages []contextkit.Message) []contextkit.Message {
	out := make([]contextkit.Message, 0, len(messages))
	for _, msg := range messages {
		out = append(out, cloneMessage(msg))
	}
	return out
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	copied := make(map[string]any, len(metadata))
	for k, v := range metadata {
		copied[k] = v
	}
	return copied
}

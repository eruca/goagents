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
	"sync"
	"time"

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
	// catalogHeader starts the list of dropped steps a summary carries.
	catalogHeader = "earlier steps (newest last"
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

	// KeepRecentReasoning is how many of the newest assistant messages keep
	// their reasoning when older reasoning is cleared: zero means 1, and a
	// negative value never clears reasoning.
	KeepRecentReasoning int
	// KeepRecentToolResults is how many of the newest tool results keep
	// their content when older ones are cleared: zero means 3, and a
	// negative value never clears tool results.
	KeepRecentToolResults int
	// Offload, when set, keeps the full content of each tool result that is
	// truncated to MaxToolChars or cleared, of each message dropped behind
	// the summary, and of reasoning that is cleared; the projection and the
	// summary's catalog name their references.
	Offload contextkit.OffloadStore
	// CatalogEntries bounds the dropped steps a summary lists, newest kept;
	// zero means 12.
	CatalogEntries int
	// ClearTarget is the share of the budget clearing tool results aims
	// for once it has to start, so the following calls keep the same
	// cleared prefix instead of clearing one more result each time; zero
	// means 0.7, and 1 clears only until the conversation fits.
	ClearTarget float64
	// CompactTarget is the share of the window's space the kept steps may
	// fill when a Summarizer writes the summary, so the following calls fit
	// without summarizing again; zero means 0.5, and 1 fills it.
	CompactTarget float64
	// SummarizerFailureLimit consecutive Summarizer failures pause it for
	// SummarizerCooldown; zero means 3 and 5 minutes. A failed or paused
	// Summarizer falls back to the placeholder summary.
	SummarizerFailureLimit int
	SummarizerCooldown     time.Duration
	// Now is the clock for the cooldown; nil means time.Now.
	Now func() time.Time
}

// Compressor fits a conversation into a budget in steps, cheapest first, and
// stops as soon as it fits (see contextkit.Step):
//
//  1. tool results over MaxToolChars are truncated, always;
//  2. older reasoning is cleared, keeping KeepRecentReasoning;
//  3. older tool results are cleared, oldest first, keeping
//     KeepRecentToolResults, the tool call, and its reference;
//  4. older steps are dropped behind one summary message, written by the
//     Summarizer in deep mode;
//  5. when the newest step alone is too large, its longest tool results and
//     reasoning are truncated.
//
// The budget covers system messages, the summary, and the kept conversation,
// in characters, tokens, or both.
//
// Compress is incremental: feed it its previous output plus the new messages
// and only the newly dropped messages are summarized, together with the
// previous summary, instead of the whole history again. A Compressor is safe
// for concurrent use.
type Compressor struct {
	cfg Config

	mu          sync.Mutex
	failures    int
	pausedUntil time.Time
}

func New(cfg Config) *Compressor {
	if cfg.Profile == "" {
		cfg.Profile = contextkit.ProfileStandard
	}
	if cfg.MinRecent <= 0 {
		cfg.MinRecent = 1
	}
	if cfg.KeepRecentReasoning == 0 {
		cfg.KeepRecentReasoning = 1
	}
	if cfg.KeepRecentToolResults == 0 {
		cfg.KeepRecentToolResults = 3
	}
	if cfg.CompactTarget <= 0 || cfg.CompactTarget > 1 {
		cfg.CompactTarget = 0.5
	}
	if cfg.ClearTarget <= 0 || cfg.ClearTarget > 1 {
		cfg.ClearTarget = 0.7
	}
	if cfg.CatalogEntries <= 0 {
		cfg.CatalogEntries = 12
	}
	if cfg.SummarizerFailureLimit <= 0 {
		cfg.SummarizerFailureLimit = 3
	}
	if cfg.SummarizerCooldown <= 0 {
		cfg.SummarizerCooldown = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
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

	result := &contextkit.Result{Profile: profile, Levels: contextkit.LevelsForProfile(profile)}
	messages, truncated := c.budgetToolMessages(ctx, req.Messages)
	if truncated {
		result.Applied = append(result.Applied, contextkit.StepToolBudget)
	}
	system, previous, conversation := split(messages)
	costOf := c.coster(budget)
	base := costOf(system)
	fitsWhole := func(conversation []contextkit.Message) bool {
		return !budget.Bounded() || budget.fits(base.add(costOf(previous)).add(costOf(conversation)))
	}

	// The cheap steps keep every message and its place in the conversation.
	if !fitsWhole(conversation) && c.cfg.KeepRecentReasoning > 0 {
		var cleared []contextkit.Message
		conversation, cleared = clearReasoning(conversation, c.cfg.KeepRecentReasoning)
		for _, original := range cleared {
			c.offload(ctx, original)
		}
		if len(cleared) > 0 {
			result.Applied = append(result.Applied, contextkit.StepClearReasoning)
			result.Cleared = append(result.Cleared, cleared...)
		}
	}
	if !fitsWhole(conversation) && c.cfg.KeepRecentToolResults > 0 {
		var cleared []contextkit.Message
		target := budget.scale(c.cfg.ClearTarget)
		belowTarget := func(conversation []contextkit.Message) bool {
			return target.fits(base.add(costOf(previous)).add(costOf(conversation)))
		}
		conversation, cleared = c.clearToolResults(ctx, conversation, belowTarget)
		if len(cleared) > 0 {
			result.Applied = append(result.Applied, contextkit.StepClearToolResults)
			result.Cleared = append(result.Cleared, cleared...)
		}
	}

	reserve := c.summaryReserve(budget)
	summarizing := c.summarizerReady(profile)
	windowBudget := budget
	if reserve.Bounded() {
		windowBudget = budget.minus(reserve)
	}
	if summarizing {
		// Summarizing is the costly step: leave room so the next calls fit
		// without it.
		windowBudget = windowBudget.scale(c.cfg.CompactTarget)
	}
	// The window leaves room for the summary: a Summarizer's reserve, or the
	// placeholder the dropped messages would get.
	fits := func(dropped []contextkit.Message, kept cost) bool {
		withSummary := base.add(kept)
		if reserve.Bounded() {
			return windowBudget.fits(withSummary)
		}
		if len(dropped) > 0 || len(previous) > 0 {
			withSummary = withSummary.add(costOf([]contextkit.Message{c.placeholderFor(previous, dropped, c.cfg.Offload != nil, budget)}))
		}
		return windowBudget.fits(withSummary)
	}
	dropped, recent := []contextkit.Message(nil), conversation
	if !fitsWhole(conversation) {
		dropped, recent, _ = windowStart(conversation, c.cfg.MinRecent, costOf, fits)
	}
	result.Dropped = len(dropped)
	if c.cfg.Offload != nil {
		dropped = cloneMessages(dropped)
		for index := range dropped {
			dropped[index].Ref = c.offload(ctx, dropped[index])
		}
	}

	out := append([]contextkit.Message(nil), system...)
	var summaries []contextkit.Message
	switch {
	case len(dropped) > 0:
		result.Applied = append(result.Applied, contextkit.StepWindow)
		summary, err := c.summarize(ctx, profile, summarizing, previous, dropped, recent, budget, reserve, req.Metadata, result)
		if err != nil {
			return nil, err
		}
		summaries = []contextkit.Message{summary}
		result.Summary = summary.Content
	case len(previous) > 0:
		summaries = previous
		result.Summary = previous[len(previous)-1].Content
	}
	fixed := base.add(costOf(summaries))
	if budget.Bounded() && !budget.fits(fixed.add(costOf(recent))) {
		var cleared []contextkit.Message
		recent, cleared = truncateToFit(recent, func(kept []contextkit.Message) bool { return budget.fits(fixed.add(costOf(kept))) })
		if len(cleared) > 0 {
			result.Applied = append(result.Applied, contextkit.StepTruncate)
			result.Cleared = append(result.Cleared, cleared...)
		}
	}
	out = append(out, summaries...)
	out = append(out, recent...)

	result.Messages = out
	result.OverBudget = budget.Bounded() && !budget.fits(fixed.add(costOf(recent)))
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
// placeholderFor builds the placeholder; assumeRefs sizes the catalog as if
// every dropped message had a reference, for messages not offloaded yet. The
// catalog takes at most catalogShare of the budget.
func (c *Compressor) placeholderFor(previous, dropped []contextkit.Message, assumeRefs bool, budget limits) contextkit.Message {
	total, images := len(dropped), imageCount(dropped)
	for _, msg := range previous {
		total += summaryCount(msg, droppedTotalKey, omittedPattern)
		images += summaryCount(msg, droppedImagesKey, imagesPattern)
	}
	content := fmt.Sprintf("%s%d messages omitted", placeholderPrefix, total)
	if images > 0 {
		content += fmt.Sprintf(" (%d images)", images)
	}
	costOf := c.coster(budget)
	share := budget.scale(catalogShare)
	fitsShare := func(catalog string) bool {
		return !budget.Bounded() || share.fits(costOf([]contextkit.Message{{Role: contextkit.RoleSystem, Content: catalog}}))
	}
	if catalog := c.catalog(previous, dropped, assumeRefs, fitsShare); catalog != "" {
		content += "\n" + catalog
	}
	summary := contextkit.Message{
		Role:     contextkit.RoleSystem,
		Content:  content,
		Metadata: map[string]any{kindKey: kindWindowSummary, droppedTotalKey: total},
	}
	if images > 0 {
		summary.Metadata[droppedImagesKey] = images
	}
	return summary
}

// fitSummary shortens a summary's content until it fits the reserve.
func (c *Compressor) fitSummary(msg contextkit.Message, reserve limits, costOf func([]contextkit.Message) cost) contextkit.Message {
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
func (c *Compressor) summarize(ctx context.Context, profile contextkit.Profile, summarizing bool, previous, dropped, recent []contextkit.Message, budget, reserve limits, metadata map[string]any, result *contextkit.Result) (contextkit.Message, error) {
	summary := c.placeholderFor(previous, dropped, false, budget)
	if profile != contextkit.ProfileDeep {
		return c.fitSummary(summary, reserve, c.coster(budget)), nil
	}
	if c.cfg.Summarizer != nil && !summarizing {
		result.SummaryFallback = true
	}

	if summarizing {
		compacted, err := c.cfg.Summarizer.Summarize(ctx, contextkit.SummarizeRequest{
			Previous:      joinContents(previous),
			Collapsed:     cloneMessages(dropped),
			Recent:        cloneMessages(recent),
			Budget:        budget.Budget,
			SummaryBudget: reserve.Budget,
			Metadata:      cloneMetadata(metadata),
		})
		if contextErr := dependencyContextError(ctx, err); contextErr != nil {
			return contextkit.Message{}, contextErr
		}
		c.recordSummarizer(err == nil)
		if err != nil {
			// A failed summary must not fail the model call: fall back to
			// the placeholder.
			result.SummaryFallback = true
		} else if compacted != nil && compacted.Summary != "" {
			result.Applied = append(result.Applied, contextkit.StepSummarize)
			total := summary.Metadata[droppedTotalKey].(int)
			header := fmt.Sprintf("%s%d messages omitted", deepSummaryPrefix, total)
			if images, _ := summary.Metadata[droppedImagesKey].(int); images > 0 {
				header += fmt.Sprintf(", %d images", images)
			}
			header += "):\n"
			// The catalog of dropped steps follows the summary, so their
			// references stay listed; truncation cuts it first.
			_, catalog, _ := strings.Cut(summary.Content, "\n")
			body := compacted.Summary
			if catalog != "" {
				body += "\n\n" + catalog
			}
			summary.Content = header + body
			summary = c.fitSummary(summary, reserve, c.coster(budget))
			kept, _, _ := strings.Cut(strings.TrimPrefix(summary.Content, header), "\n\n"+catalogHeader)
			result.AutoCompact = &contextkit.Compact{
				Summary:  kept,
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

// budgetToolMessages projects tool messages and truncates results over
// MaxToolChars, offloading their full content first when an OffloadStore is
// set. It reports whether it truncated any.
func (c *Compressor) budgetToolMessages(ctx context.Context, messages []contextkit.Message) ([]contextkit.Message, bool) {
	out := make([]contextkit.Message, 0, len(messages))
	truncated := false
	for _, msg := range messages {
		copied := cloneMessage(msg)
		if copied.Role == contextkit.RoleTool && !isProjected(copied) {
			limit := c.cfg.MaxToolChars
			if isReadBack(copied) {
				// Content read back from an archive is already bounded and
				// archived; truncating it again would defeat the read.
				limit = 0
			} else if limit > 0 && len([]rune(copied.Content)) > limit {
				copied.Ref = c.offload(ctx, copied)
			}
			copied = toolprojection.Project(copied, toolprojection.Config{MaxResultChars: limit})
			truncated = truncated || copied.Metadata["contextkit.tool_truncated"] == true
		}
		out = append(out, copied)
	}
	return out, truncated
}

// ReadBackKey marks a tool message whose content was read back from an
// Archive: compression does not truncate or offload it again.
const ReadBackKey = "contextkit.read_back"

func isReadBack(msg contextkit.Message) bool {
	readBack, _ := msg.Metadata[ReadBackKey].(bool)
	return readBack
}

func isProjected(msg contextkit.Message) bool {
	projected, _ := msg.Metadata["contextkit.tool_projected"].(bool)
	return projected
}

// offload stores a tool result's content and returns its reference: the one
// it already has, a new one, or none when no store is set or it fails.
func (c *Compressor) offload(ctx context.Context, msg contextkit.Message) string {
	if msg.Ref != "" || c.cfg.Offload == nil || isReadBack(msg) {
		return msg.Ref
	}
	ref, err := c.cfg.Offload.Offload(ctx, cloneMessage(msg))
	if err != nil {
		return ""
	}
	return ref
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

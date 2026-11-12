package brain

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/vvchaschin/communitybot/internal/llm"
	"github.com/vvchaschin/communitybot/internal/storage"
)

// The model evaluates every new message of a chat once per batch_interval_seconds (SPEC 4.3): one
// request per batch, a decision per message — a reaction, a reply or nothing. Instructions of the
// leadership ("❤ on my messages", "on any mention of X answer «…»") are carried out by the model
// too; the code only checks what it may do.

const (
	// maxBatch caps the new messages shown to the model at once; with an hourly batch a busy chat
	// may have more, and then only the latest are evaluated (the rest stay history).
	maxBatch = 200
	// maxRepliesPerBatch caps the replies one evaluation sends (instruction replies included), so a
	// batch full of triggers does not turn into a stream of bot messages.
	maxRepliesPerBatch = 3
)

// replyPolicy says which new messages the bot may answer on its own (not by an instruction).
type replyPolicy struct {
	any   bool  // any message
	only  []int // otherwise only these ids (empty: none)
	quiet bool  // the group's mode allows reactions only: no replies at all, by instructions neither (SPEC 4.1a)
}

func (p replyPolicy) allows(id int) bool { return p.any || slices.Contains(p.only, id) }

// evaluate asks the model how to respond to the chat's new messages and does it.
func (b *Brain) evaluate(ctx context.Context, chatID int64) {
	l := b.lock(chatID)
	if !l.TryLock() {
		return // stays pending, next batch
	}
	defer l.Unlock()
	b.evaluateLocked(ctx, chatID)
}

// inDialogNow: the message's author talks with the bot in this topic and the message may belong to
// the bot's thread (it is there, or in no thread yet) — see dialogTargets.
func (b *Brain) inDialogNow(ctx context.Context, m storage.Message) bool {
	cur, err := b.store.Get(ctx, m.ChatID, m.MessageID)
	if err != nil || cur == nil {
		return false
	}
	return b.dialogTargets(ctx, m.ChatID, map[int]storage.Message{cur.MessageID: *cur})[cur.MessageID]
}

func (b *Brain) evaluateLocked(ctx context.Context, chatID int64) {
	b.mu.Lock()
	delete(b.pending, chatID)
	b.mu.Unlock()

	st, err := b.store.ChatState(ctx, chatID)
	if err != nil {
		slog.Error("chat state", "err", err)
		return
	}
	fresh, err := b.store.Fresh(ctx, chatID, st.LastEvaluatedID, maxBatch)
	if err != nil || len(fresh) == 0 {
		return
	}
	// Older messages of these topics may have left the model's window: into the summary with them.
	defer b.summarizeTopics(ctx, chatID, topicsOf(fresh))
	// Marked evaluated before asking: when the model is unavailable, batches do not pile up (SPEC 6).
	if err := b.store.SetLastEvaluated(ctx, chatID, fresh[len(fresh)-1].MessageID); err != nil {
		slog.Error("set last evaluated", "err", err)
	}
	mode := b.mode(ctx, chatID)
	if mode == storage.ModeSilent {
		return // nothing to do in a silent group: the batch is only seen (SPEC 4.1a)
	}
	defer func() {
		if ctx.Err() == nil {
			return
		}
		// A dialog evaluation cancelled by the person's next message before the bot did anything:
		// the messages are new again, for the restarted one (dialog.go).
		slog.Info("evaluation cancelled", "chat_id", chatID)
		if err := b.store.SetLastEvaluated(context.WithoutCancel(ctx), chatID, st.LastEvaluatedID); err != nil {
			slog.Error("set last evaluated", "err", err)
		}
		b.markPending(chatID)
	}()

	// Messages addressed to the bot are answered separately; its own are not targets either.
	targets := map[int]storage.Message{}
	threads := map[int]bool{}
	for _, m := range fresh {
		if !m.IsSelf && !m.Addressed {
			targets[m.MessageID] = m
			threads[m.ThreadID] = true
		}
	}
	if len(targets) == 0 {
		return
	}
	if !b.llm.Available(ctx, llm.RoleFilter, llm.Initiative) {
		slog.Debug("evaluation skipped: no quota", "chat_id", chatID)
		return
	}
	earlier, err := b.store.Before(ctx, chatID, fresh[0].MessageID, b.cfg.ContextMessages)
	if err != nil {
		slog.Error("load history", "err", err)
		return
	}

	// Replying to someone who is talking with the bot is answering, not interjecting: the cooldown
	// neither applies nor starts. The cooldown and the monologue guard limit the bot's own replies
	// only: reactions and instructions are always carried out.
	dialog := b.dialogTargets(ctx, chatID, targets)
	policy := replyPolicy{any: true}
	cooldown := time.Duration(b.cfg.CooldownMinutes * float64(time.Minute))
	if !st.LastInitiativeAt.IsZero() && b.now().Sub(st.LastInitiativeAt) < cooldown {
		policy = replyPolicy{only: sortedIDs(dialog)}
		slog.Debug("evaluation: cooldown, own replies only within a dialog", "chat_id", chatID, "dialog", policy.only)
	}
	all := append(slices.Clone(earlier), fresh...)
	if ownStreak(all) >= b.cfg.MaxConsecutiveOwn {
		slog.Debug("evaluation: own messages in a row, no own replies", "chat_id", chatID)
		policy = replyPolicy{}
	}
	if mode == storage.ModeReactions {
		policy = replyPolicy{quiet: true}
	}

	view, err := b.evalView(ctx, chatID, earlier, fresh, targets)
	if err != nil {
		slog.Error("load threads", "chat_id", chatID, "err", err)
		return
	}
	mem := b.memory(ctx, chatID, sortedIDs(threads)...)
	b.withNotes(ctx, &mem, chatID, evaluationPeople(fresh, targets), maxNotesInEvaluate)
	b.selectLore(ctx, &mem, llm.Initiative, evaluatePrompt(b.personaOf(mem), mem, view, policy))
	raw, provider, err := b.llm.Complete(llm.WithSchema(ctx, evaluateSchema(len(view.threads) > 0)), llm.RoleFilter,
		llm.Initiative, evaluatePrompt(b.personaOf(mem), mem, view, policy))
	if err != nil {
		if !errors.Is(err, llm.ErrUnavailable) && ctx.Err() == nil {
			slog.Error("evaluation", "err", err)
		}
		return
	}
	acts, err := parseActions(raw)
	if err != nil {
		slog.Warn("evaluation returned unparsable answer", "provider", provider, "err", err)
		return
	}
	slog.Info("evaluation", "chat_id", chatID, "messages", len(fresh), "actions", len(acts), "threads", len(view.threads),
		"provider", provider)
	// Before replying: a reply is written in the context of its target's thread.
	b.applyAssignments(ctx, chatID, parseAssignments(raw), view, targets)

	b.act(ctx, chatID, acts, targets, policy, dialog, mem)
}

// topicsOf lists the topics of the messages.
func topicsOf(ms []storage.Message) []int {
	set := map[int]bool{}
	for _, m := range ms {
		set[m.ThreadID] = true
	}
	return sortedIDs(set)
}

// evalView is a batch split into threads and the flow outside them (SPEC 7a).
type evalView struct {
	threads        []threadView      // threads with new messages, and active ones new messages may continue
	earlier, fresh []storage.Message // outside the threads: before the batch and the batch itself
	isNew          func(storage.Message) bool
}

// evalView shows every thread a new message is in, with a few earlier messages of it, and the
// topics' active threads that a new message without a reply may continue.
func (b *Brain) evalView(ctx context.Context, chatID int64, earlier, fresh []storage.Message,
	targets map[int]storage.Message) (evalView, error) {
	first := fresh[0].MessageID
	v := evalView{isNew: func(m storage.Message) bool { return m.MessageID >= first }}
	inThread := map[int]int{} // root -> new messages in it
	for _, m := range fresh {
		if m.RootID != 0 {
			inThread[m.RootID]++
		}
	}
	topics := map[int]bool{}
	for _, m := range targets {
		if unthreaded(m) {
			topics[m.ThreadID] = true
		}
	}
	for _, topic := range sortedIDs(topics) {
		roots, err := b.activeThreads(ctx, chatID, topic)
		if err != nil {
			return v, err
		}
		for _, r := range roots {
			if _, ok := inThread[r]; !ok {
				inThread[r] = 0
			}
		}
	}
	shown := map[int]bool{}
	for _, r := range slices.Sorted(maps.Keys(inThread)) {
		t, err := b.loadThread(ctx, chatID, r, candidateTail+inThread[r])
		if err != nil {
			return v, err
		}
		if len(t.msgs) == 0 {
			continue
		}
		v.threads = append(v.threads, t)
		for _, m := range t.msgs {
			shown[m.MessageID] = true
		}
	}
	outside := func(ms []storage.Message) []storage.Message {
		return slices.DeleteFunc(slices.Clone(ms), func(m storage.Message) bool { return shown[m.MessageID] })
	}
	v.earlier, v.fresh = outside(earlier), outside(fresh)
	return v, nil
}

// unthreaded: a message people wrote, without a reply and in no thread — it may continue one by meaning.
func unthreaded(m storage.Message) bool {
	return m.RootID == 0 && m.ReplyTo == 0 && m.Kind == "" && !m.IsSelf
}

// applyAssignments puts new messages without a reply into the threads the evaluation says they
// continue: only a shown thread of the same topic that started before the message.
func (b *Brain) applyAssignments(ctx context.Context, chatID int64, as []assignment, v evalView, targets map[int]storage.Message) {
	for _, a := range as {
		m, ok := targets[a.ID]
		i := slices.IndexFunc(v.threads, func(t threadView) bool { return t.root == a.Thread })
		if !ok || !unthreaded(m) || i < 0 || v.threads[i].topic != m.ThreadID || a.Thread >= m.MessageID {
			slog.Debug("thread assignment skipped", "chat_id", chatID, "id", a.ID, "thread", a.Thread)
			continue
		}
		b.setRoot(ctx, m, a.Thread, "evaluation")
	}
}

// plannedReply is a reply the evaluation decided on and the code allowed.
type plannedReply struct {
	target storage.Message
	act    action
	rule   *storage.Lore // the instruction that requires it, if any
	own    bool          // the bot's own initiative: starts the cooldown
}

// act carries out the evaluator's actions within what the bot may do now.
func (b *Brain) act(ctx context.Context, chatID int64, acts []action, targets map[int]storage.Message,
	policy replyPolicy, dialog map[int]bool, mem chatMemory) {
	reacted := map[int]bool{}
	replied := map[int]bool{}
	hits := map[ruleHit]bool{} // instruction replies planned in this batch
	var replies []plannedReply
	ownUsed := false
	for _, a := range acts {
		m, ok := targets[a.ID]
		if !ok {
			slog.Debug("evaluation action skipped: not a new message", "chat_id", chatID, "id", a.ID)
			continue
		}
		rule := ruleFor(mem, a.Rule, m)
		if emoji := chosenReaction(a.React); emoji != "" && !reacted[a.ID] {
			if !b.commit(ctx) {
				return
			}
			reacted[a.ID] = true
			if !isAllowedReaction(emoji) {
				emoji = "👍"
			}
			slog.Info("evaluation reaction", "chat_id", chatID, "message_id", m.MessageID, "emoji", emoji,
				"rule", ruleID(rule), "reason", a.Reason)
			if err := b.out.React(ctx, chatID, m.MessageID, emoji); err != nil {
				slog.Warn("react failed", "err", err)
			}
		}
		if !a.Reply || replied[a.ID] || len(replies) >= maxRepliesPerBatch {
			continue
		}
		if policy.quiet {
			slog.Info("evaluation wanted to reply, but the group allows reactions only", "chat_id", chatID,
				"target", m.MessageID, "rule", ruleID(rule), "reason", a.Reason)
			continue
		}
		p := plannedReply{target: m, act: a, rule: rule}
		switch {
		case rule != nil:
			hit := ruleHit{rule.ID, m.UserID}
			if hits[hit] || b.ruleHitRecently(rule.ID, m.UserID) {
				slog.Info("instruction reply skipped: answered this author recently", "chat_id", chatID,
					"message_id", m.MessageID, "rule", rule.ID)
				continue
			}
			hits[hit] = true
		case dialog[a.ID] && policy.allows(a.ID) && b.inBotThread(ctx, m):
		case policy.any && !ownUsed: // in the cooldown only a dialog may be answered
			p.own, ownUsed = true, true
		default:
			slog.Info("evaluation wanted to reply, but replying to this message is not allowed now",
				"chat_id", chatID, "target", m.MessageID, "reason", a.Reason)
			continue
		}
		replied[a.ID] = true
		replies = append(replies, p)
	}
	for _, p := range replies {
		b.reply(ctx, chatID, p)
	}
}

// reply sends one planned reply: the instruction's exact phrase, or a text the model writes.
func (b *Brain) reply(ctx context.Context, chatID int64, p plannedReply) {
	m := p.target
	if b.repliesForbidden(ctx, m) {
		return
	}
	slog.Info("evaluation reply", "chat_id", chatID, "target", m.MessageID, "rule", ruleID(p.rule), "reason", p.act.Reason)
	var text string
	if phrase := strings.TrimSpace(p.act.Text); p.rule != nil && phrase != "" && strings.Contains(p.rule.Text, phrase) {
		// The phrase must come from the instruction itself: the model may not put words in its mouth.
		stop := b.keepTyping(ctx, m.ChatID, m.ThreadID)
		b.sleep(ctx, b.replyDelay())
		stop()
		text = phrase
	} else {
		prio, reason := llm.Initiative, p.act.Reason
		if p.rule != nil {
			prio = llm.Direct
			reason = "Выполни указание руководства: " + p.rule.Text + ". " + reason
		}
		if !b.llm.Available(ctx, llm.RoleReply, prio) {
			return
		}
		var err error
		if text, err = b.generate(ctx, m, reason, prio); err != nil {
			if !errors.Is(err, llm.ErrUnavailable) && ctx.Err() == nil {
				slog.Error("generate reply", "err", err)
			}
			return
		}
	}
	if !b.commit(ctx) {
		return
	}
	if !b.send(ctx, m, text) {
		return
	}
	if p.rule != nil {
		b.recordRuleHit(p.rule.ID, m.UserID)
	}
	if p.own {
		if err := b.store.SetLastInitiative(ctx, chatID, b.now()); err != nil {
			slog.Error("set last initiative", "err", err)
		}
	}
}

// ruleFor finds the instruction an action cites, if it applies to the message's topic.
func ruleFor(mem chatMemory, id int64, m storage.Message) *storage.Lore {
	if id == 0 {
		return nil
	}
	for i, l := range mem.lore {
		if l.ID == id && l.Kind == storage.LoreInstruction && (l.ChatWide || l.ThreadID == m.ThreadID) {
			return &mem.lore[i]
		}
	}
	return nil
}

func ruleID(l *storage.Lore) int64 {
	if l == nil {
		return 0
	}
	return l.ID
}

func (b *Brain) ruleHitRecently(rule, author int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.ruleHits[ruleHit{rule, author}]
	return ok && b.now().Sub(at) < ruleHitTTL
}

func (b *Brain) recordRuleHit(rule, author int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ruleHits[ruleHit{rule, author}] = b.now()
}

// dialogTargets marks messages whose authors talked with the bot (addressed it or got its reply)
// in the same topic within dialog_minutes before the message.
// Someone the bot talked with may go on without a reply or its name — but only in the bot's
// thread (SPEC 7a): a new question to the whole chat right after is not a dialog. A message in no
// thread yet is a candidate: the evaluation may put it into the bot's thread (checked again in act).
func (b *Brain) dialogTargets(ctx context.Context, chatID int64, targets map[int]storage.Message) map[int]bool {
	window := time.Duration(b.cfg.DialogMinutes * float64(time.Minute))
	out := map[int]bool{}
	for id, m := range targets {
		in, err := b.store.InDialog(ctx, chatID, m.ThreadID, m.UserID, m.Date.Add(-window))
		if err != nil {
			slog.Error("dialog check", "chat_id", chatID, "err", err)
		}
		if in && (m.RootID == 0 || b.withBot(ctx, chatID, m.RootID)) {
			out[id] = true
		}
	}
	return out
}

// withBot reports whether the bot speaks in the thread `root`.
func (b *Brain) withBot(ctx context.Context, chatID int64, root int) bool {
	if root == 0 {
		return false
	}
	in, err := b.store.SelfInThread(ctx, chatID, root)
	if err != nil {
		slog.Error("thread check", "chat_id", chatID, "err", err)
	}
	return in
}

// inBotThread: the message is now (after thread assignment) in a thread where the bot speaks.
func (b *Brain) inBotThread(ctx context.Context, m storage.Message) bool {
	cur, err := b.store.Get(ctx, m.ChatID, m.MessageID)
	if err != nil || cur == nil {
		return false
	}
	return b.withBot(ctx, m.ChatID, cur.RootID)
}

package usagehistory

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"time"
)

type transcriptParser struct {
	provider      string
	session       string
	model         string
	tier          string
	sawMeta       bool
	forkPrelude   bool
	forkAnchor    time.Time
	previous      *codexTokenUsage
	lastSignature string
}

type transcriptEnvelope struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	RequestID string          `json:"requestId"`
	SessionID string          `json:"sessionId"`
	Message   json.RawMessage `json:"message"`
	Payload   json.RawMessage `json:"payload"`
}

func (p *transcriptParser) parse(line []byte) (*importRecord, bool) {
	if p.provider == "claude" && !bytes.Contains(line, []byte(`"usage"`)) {
		return nil, false
	}
	if p.provider == "codex" && !bytes.Contains(line, []byte(`"token_count"`)) &&
		!bytes.Contains(line, []byte(`"turn_context"`)) && !bytes.Contains(line, []byte(`"session_meta"`)) &&
		!bytes.Contains(line, []byte(`"thread_settings_applied"`)) {
		return nil, false
	}
	var envelope transcriptEnvelope
	if errJSON := json.Unmarshal(line, &envelope); errJSON != nil {
		return nil, true
	}
	if p.provider == "claude" {
		return parseClaudeImport(envelope)
	}
	return p.parseCodex(envelope)
}

func parseClaudeImport(envelope transcriptEnvelope) (*importRecord, bool) {
	if envelope.Type != "assistant" {
		return nil, false
	}
	var message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input  int64  `json:"input_tokens"`
			Cache  int64  `json:"cache_read_input_tokens"`
			Write  int64  `json:"cache_creation_input_tokens"`
			Output int64  `json:"output_tokens"`
			Speed  string `json:"speed"`
		} `json:"usage"`
	}
	if errJSON := json.Unmarshal(envelope.Message, &message); errJSON != nil {
		return nil, true
	}
	if message.Model == "<synthetic>" || message.Usage == nil {
		return nil, false
	}
	u := message.Usage
	if envelope.Timestamp.IsZero() || message.Model == "" || (message.ID == "" && envelope.RequestID == "") ||
		!validTokenSum(u.Input, u.Cache, u.Write, u.Output) {
		return nil, true
	}
	if u.Input+u.Cache+u.Write+u.Output == 0 {
		return nil, false
	}
	tier := TierStandard
	if u.Speed == "fast" {
		tier = TierFast
	}
	return &importRecord{
		id: importID("claude", message.ID, envelope.RequestID), at: envelope.Timestamp,
		provider: "claude", model: message.Model, tier: tier,
		counters: Counters{Requests: 1, UncachedInput: u.Input, CacheRead: u.Cache, CacheWrite: u.Write, Output: u.Output},
	}, false
}

// Codex counts cached input inside input and reasoning inside output.
type codexTokenUsage struct {
	Input     *int64 `json:"input_tokens"`
	Cached    int64  `json:"cached_input_tokens"`
	Write     int64  `json:"cache_write_input_tokens"`
	Output    *int64 `json:"output_tokens"`
	Reasoning int64  `json:"reasoning_output_tokens"`
	Total     *int64 `json:"total_tokens"`
}

func (u *codexTokenUsage) values() ([6]int64, bool) {
	if u == nil || u.Input == nil || u.Output == nil ||
		!validTokenSum(*u.Input, *u.Output) || !validTokenSum(u.Cached, u.Write) ||
		u.Cached+u.Write > *u.Input || u.Reasoning < 0 || u.Reasoning > *u.Output {
		return [6]int64{}, false
	}
	total := *u.Input + *u.Output
	if u.Total != nil {
		if *u.Total < total {
			return [6]int64{}, false
		}
		total = *u.Total
	}
	return [6]int64{*u.Input, u.Cached, u.Write, *u.Output, u.Reasoning, total}, true
}

func validTokenSum(values ...int64) bool {
	var sum int64
	for _, value := range values {
		if value < 0 || value > math.MaxInt64-sum {
			return false
		}
		sum += value
	}
	return true
}

func usageSignature(values [6]int64) string {
	signature := ""
	for _, value := range values {
		signature += strconv.FormatInt(value, 10) + ":"
	}
	return signature
}

func (p *transcriptParser) parseCodex(envelope transcriptEnvelope) (*importRecord, bool) {
	var payload struct {
		Type       string          `json:"type"`
		ID         string          `json:"id"`
		SessionID  string          `json:"session_id"`
		Model      string          `json:"model"`
		ForkedFrom string          `json:"forked_from_id"`
		Source     json.RawMessage `json:"source"`
		Settings   struct {
			Tier string `json:"service_tier"`
		} `json:"thread_settings"`
		Info *struct {
			Last       *codexTokenUsage `json:"last_token_usage"`
			Cumulative *codexTokenUsage `json:"total_token_usage"`
		} `json:"info"`
	}
	if errJSON := json.Unmarshal(envelope.Payload, &payload); errJSON != nil {
		return nil, true
	}
	switch envelope.Type {
	case "session_meta":
		if !p.sawMeta {
			var source struct {
				Subagent struct {
					Spawn struct {
						Parent string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subagent"`
			}
			// CLI sessions use a string source; subagent sources carry the parent identity.
			_ = json.Unmarshal(payload.Source, &source)
			p.sawMeta, p.session = true, payload.ID
			if p.session == "" {
				p.session = payload.SessionID
			}
			p.forkPrelude = payload.ForkedFrom != "" || source.Subagent.Spawn.Parent != ""
			p.forkAnchor = envelope.Timestamp
		}
		return nil, false
	case "turn_context":
		if payload.Model != "" {
			p.model = payload.Model
		}
		return nil, false
	}
	if payload.Type == "thread_settings_applied" {
		p.tier = TierStandard
		if payload.Settings.Tier == "priority" || payload.Settings.Tier == "fast" {
			p.tier = TierFast
		}
		return nil, false
	}
	if payload.Type != "token_count" || payload.Info == nil {
		return nil, false
	}
	// A later re-emission may supply missing context; do not consume its counters yet.
	if envelope.Timestamp.IsZero() || p.model == "" || p.session == "" {
		return nil, true
	}
	last, validLast := payload.Info.Last.values()
	cumulative, validCumulative := payload.Info.Cumulative.values()
	previous, hadPrevious := p.previous.values()
	if validCumulative {
		p.previous = payload.Info.Cumulative
	}
	if p.forkPrelude {
		// Fork rollouts copy the parent's billed history in a short synchronous burst.
		if !envelope.Timestamp.IsZero() && envelope.Timestamp.Sub(p.forkAnchor) < time.Second {
			p.forkAnchor = envelope.Timestamp
			if validLast {
				p.advanceLegacy(payload.Info.Cumulative == nil, last)
			}
			return nil, false
		}
		p.forkPrelude, p.lastSignature = false, ""
	}
	if !validLast || (payload.Info.Cumulative != nil && !validCumulative) {
		return nil, true
	}
	if validCumulative && hadPrevious && cumulative == previous {
		return nil, false
	}
	// Synthetic context-fill events carry a total without any billed token components.
	if last[0]+last[3] == 0 {
		p.lastSignature = ""
		return nil, false
	}
	if last[5] != last[0]+last[3] {
		return nil, true
	}
	identity := "legacy:" + usageSignature(last)
	if validCumulative {
		for i := range last {
			if cumulative[i] < previous[i] || cumulative[i]-previous[i] != last[i] {
				return nil, true
			}
		}
		identity = "cumulative:" + usageSignature(cumulative)
	} else {
		if !p.advanceLegacy(true, last) {
			return nil, false
		}
	}
	identity += envelope.Timestamp.UTC().Format(time.RFC3339Nano)
	p.lastSignature = usageSignature(last)
	tier := p.tier
	if tier == "" {
		tier = TierStandard
	}
	return &importRecord{
		id: importID("codex", p.session, identity), at: envelope.Timestamp,
		provider: "codex", model: p.model, tier: tier,
		counters: Counters{Requests: 1, UncachedInput: last[0] - last[1] - last[2],
			CacheRead: last[1], CacheWrite: last[2], Output: last[3], Reasoning: last[4]},
	}, false
}

func (p *transcriptParser) advanceLegacy(advance bool, last [6]int64) bool {
	signature := usageSignature(last)
	if signature == p.lastSignature {
		return false
	}
	p.lastSignature = signature
	if !advance {
		return true
	}
	previous, _ := p.previous.values()
	for i := range previous {
		if last[i] > math.MaxInt64-previous[i] {
			return false
		}
		previous[i] += last[i]
	}
	p.previous = &codexTokenUsage{Input: &previous[0], Cached: previous[1], Write: previous[2],
		Output: &previous[3], Reasoning: previous[4], Total: &previous[5]}
	return true
}

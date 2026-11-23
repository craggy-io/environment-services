// Package redact keeps secret values out of what leaves the manager: a
// Secret prints as "[redacted]" in every formatting path, and a Redactor
// scrubs the values the manager is delivering from command output, errors
// and signals.
package redact

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Secret wraps a credential value. All formatting prints "[redacted]". It
// is not encryption: it keeps values out of logs, tool results and agent
// context.
type Secret struct{ v string }

// NewSecret wraps a value.
func NewSecret(v string) Secret { return Secret{v: v} }

func (s Secret) String() string               { return "[redacted]" }
func (s Secret) GoString() string             { return "[redacted]" }
func (s Secret) Format(f fmt.State, _ rune)   { fmt.Fprint(f, "[redacted]") }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (s Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Reveal returns the value. It exists for the delivery code inside this
// library; agent-facing code must never call it.
func (s Secret) Reveal() string { return s.v }

// Redactor matches the secret values the manager is delivering. It
// implements signalcore.Redactor. Matching is Aho–Corasick over raw values
// plus their base64 and URL-encoded forms; values shorter than 6 bytes are
// skipped. It knows only what it is told: Hold when a credential is
// redeemed for a holder, Forget when the holder is gone.
type Redactor struct {
	mu       sync.RWMutex
	held     map[string]map[string]map[string]Secret // holder -> cred -> field
	ac       *aho
	patterns []pattern
}

type pattern struct {
	text string
	tag  string // "[secret:cred_…/field]"
}

// New returns a redactor that matches nothing yet.
func New() *Redactor { return &Redactor{held: map[string]map[string]map[string]Secret{}} }

// Hold makes the redactor match a credential's secret fields for as long
// as holder has it.
func (r *Redactor) Hold(holder, cred string, secret map[string]Secret) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held[holder] == nil {
		r.held[holder] = map[string]map[string]Secret{}
	}
	r.held[holder][cred] = maps.Clone(secret)
	r.rebuild()
}

// Forget drops everything held for holder.
func (r *Redactor) Forget(holder string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.held[holder]; !ok {
		return
	}
	delete(r.held, holder)
	r.rebuild()
}

// rebuild makes the matcher from every held secret. The caller holds the
// lock.
func (r *Redactor) rebuild() {
	var pats []pattern
	seen := map[string]bool{}
	for _, creds := range r.held {
		for cred, fields := range creds {
			for field, s := range fields {
				val := s.v
				if len(val) < 6 {
					continue
				}
				tag := "[secret:" + cred + "/" + field + "]"
				for _, form := range []string{
					val,
					base64.StdEncoding.EncodeToString([]byte(val)),
					base64.RawStdEncoding.EncodeToString([]byte(val)),
					url.QueryEscape(val),
				} {
					if len(form) >= 6 && !seen[form] {
						seen[form] = true
						pats = append(pats, pattern{text: form, tag: tag})
					}
				}
			}
		}
	}
	// Longest first so overlapping forms prefer the full value; ties by
	// text so the matcher is the same for the same secrets.
	sort.Slice(pats, func(i, j int) bool {
		if len(pats[i].text) != len(pats[j].text) {
			return len(pats[i].text) > len(pats[j].text)
		}
		return pats[i].text < pats[j].text
	})
	texts := make([]string, len(pats))
	for i, p := range pats {
		texts[i] = p.text
	}
	r.patterns = pats
	r.ac = newAho(texts)
}

// RedactString replaces every matched secret form in s with its tag.
func (r *Redactor) RedactString(s string) string {
	r.mu.RLock()
	ac, pats := r.ac, r.patterns
	r.mu.RUnlock()
	if ac == nil || len(pats) == 0 {
		return s
	}
	type hit struct{ start, end, pat int }
	var hits []hit
	ac.scan(s, func(pat, end int) {
		hits = append(hits, hit{end - len(pats[pat].text), end, pat})
	})
	if len(hits) == 0 {
		return s
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].start != hits[j].start {
			return hits[i].start < hits[j].start
		}
		return hits[i].end > hits[j].end
	})
	var b strings.Builder
	pos := 0
	for _, h := range hits {
		if h.start < pos {
			continue
		}
		b.WriteString(s[pos:h.start])
		b.WriteString(pats[h.pat].tag)
		pos = h.end
	}
	b.WriteString(s[pos:])
	return b.String()
}

// ContainsSecret reports whether bytes hold any matched secret. Pcap bytes
// are never rewritten; captures containing a match are flagged.
func (r *Redactor) ContainsSecret(b []byte) bool {
	r.mu.RLock()
	ac := r.ac
	r.mu.RUnlock()
	if ac == nil {
		return false
	}
	found := false
	ac.scan(string(b), func(pat, end int) { found = true })
	return found
}

// Redact implements signalcore.Redactor: text payloads (strings and string
// fields of maps/structs via their common shapes) are scrubbed; byte
// payloads pass through untouched.
func (r *Redactor) Redact(stream string, data any) any {
	switch d := data.(type) {
	case string:
		return r.RedactString(d)
	case map[string]any:
		out := make(map[string]any, len(d))
		for k, val := range d {
			out[k] = r.redactValue(val)
		}
		return out
	case []string:
		out := make([]string, len(d))
		for i, s := range d {
			out[i] = r.RedactString(s)
		}
		return out
	default:
		return redactKnown(r, data)
	}
}

func (r *Redactor) redactValue(v any) any {
	if s, ok := v.(string); ok {
		return r.RedactString(s)
	}
	return v
}

// redactKnown scrubs struct payloads that expose text through a
// Text/Line/Message field via reflection-free JSON round-trip for generic
// safety: values are marshalled, scrubbed, and — only when something
// changed — returned as a generic map. Unchanged data returns as-is.
func redactKnown(r *Redactor, data any) any {
	if data == nil {
		return data
	}
	if _, ok := data.([]byte); ok {
		return data
	}
	return redactJSON(r, data)
}

// --- Aho–Corasick ---

type ahoNode struct {
	next map[rune]int
	fail int
	out  []int
}

type aho struct {
	nodes []ahoNode
	lens  []int // rune lengths per pattern
}

func newAho(patterns []string) *aho {
	a := &aho{nodes: []ahoNode{{next: map[rune]int{}}}, lens: make([]int, len(patterns))}
	for pi, p := range patterns {
		state := 0
		runes := []rune(p)
		a.lens[pi] = len(p)
		_ = runes
		for _, ch := range p {
			nxt, ok := a.nodes[state].next[ch]
			if !ok {
				nxt = len(a.nodes)
				a.nodes = append(a.nodes, ahoNode{next: map[rune]int{}})
				a.nodes[state].next[ch] = nxt
			}
			state = nxt
		}
		a.nodes[state].out = append(a.nodes[state].out, pi)
	}
	// Build failure links (BFS).
	var queue []int
	for _, s := range a.nodes[0].next {
		a.nodes[s].fail = 0
		queue = append(queue, s)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for ch, nxt := range a.nodes[cur].next {
			f := a.nodes[cur].fail
			for {
				if to, ok := a.nodes[f].next[ch]; ok && to != nxt {
					a.nodes[nxt].fail = to
					break
				}
				if f == 0 {
					a.nodes[nxt].fail = 0
					break
				}
				f = a.nodes[f].fail
			}
			a.nodes[nxt].out = append(a.nodes[nxt].out, a.nodes[a.nodes[nxt].fail].out...)
			queue = append(queue, nxt)
		}
	}
	return a
}

// scan calls fn(patternIndex, endByteIndex) for every match.
func (a *aho) scan(s string, fn func(pat, end int)) {
	state := 0
	for i, ch := range s {
		for {
			if nxt, ok := a.nodes[state].next[ch]; ok {
				state = nxt
				break
			}
			if state == 0 {
				break
			}
			state = a.nodes[state].fail
		}
		end := i + len(string(ch))
		for _, pat := range a.nodes[state].out {
			fn(pat, end)
		}
	}
}

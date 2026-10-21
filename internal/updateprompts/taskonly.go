// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package updateprompts

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/pages/internal/jsstr"
)

// A recurring prompt is the task description of ONE run. The request that
// produced it is usually phrased as a request for the prompt ("create a reusable
// daily unattended refresh … this is prompt creation only; do not install a
// schedule now"), and it used to be quoted verbatim, so every run read
// instructions about prompts and schedulers it had no part in. Production runs
// then reported on, or were held to, a scheduler they could not see, and the
// fix was another paragraph telling them to ignore it. The scheduler already
// knows when a task runs; the run only needs the data work. So, for recurring
// prompts only, sentences that are about scheduling or prompt creation are
// dropped and a leading "create a recurring daily …" framing is trimmed, and the
// caller is told exactly what was removed. The patterns are deliberately narrow:
// a report that is merely NAMED "scheduled" ("OpenX Scheduled Report", "the
// standard scheduled workbook") is a source, not a scheduling instruction.
var schedulingSentence = []*regexp.Regexp{
	jsRE(`(?i)\bprompt[- ]creation\b`),
	// A sentence that asks for the prompt itself ("Create a reusable DAILY
	// data-refresh prompt for the existing dashboard.") — the slug is already
	// the TARGET line, and the run is not making a prompt.
	jsRE(`(?i)^\s*(?:please\s+)?(?:create|generate|prepare|build|make|write|give\s+me)\b[^.!?]{0,120}\bprompts?\b`),
	jsRE(`(?i)\b(?:do\s+not|don't|never|no\s+need\s+to)\s+(?:install|create|configure|modify|set\s+up|change|schedule|register|verify|report\s+on)\b[^.;]*\b(?:schedul\w*|cron|recurrence|recurring\s+(?:job|task))`),
	jsRE(`(?i)\b(?:user'?s|user-owned|your|their|own)\s+scheduler\b`),
	jsRE(`(?i)\b(?:invoked|triggered|dispatched)\s+by\s+(?:a|the|your|their)?\s*(?:user'?s\s+)?(?:scheduler|cron)\b`),
	jsRE(`(?i)\bno\s+schedule\s+(?:was|has\s+been|is|will\s+be)\s+(?:created|installed)\b`),
	jsRE(`(?i)\b(?:install|create|set\s+up|configure)\s+(?:a|the)\s+(?:daily\s+|recurring\s+)?(?:schedule|cron\s+job)\b`),
}

const cadenceWord = `(?:reusable|recurring|repeatable|repeating|daily|weekly|hourly|nightly|unattended|scheduled|automatic|automated|auto-?updat(?:e|ing))`

var (
	// JavaScript ends this pattern with a lookahead,
	// (?=(?:data-only\s+|managed-data\s+)?(?:refresh|update|run|job|task)\b).
	// RE2 has none, so the lookahead's text is matched and the framing is the
	// first group: leftmost-first matching picks the same framing a
	// backtracking engine would.
	leadingFraming = jsRE(`(?i)^((?:please\s+)?(?:(?:create|generate|prepare|build|make|write|set\s+up)\s+(?:me\s+)?(?:a|an)\s+)?(?:` +
		cadenceWord + `[` + jsWS + `,]+)+)(?:data-only\s+|managed-data\s+)?(?:refresh|update|run|job|task)\b`)
	leadingRunFraming = jsRE(`(?i)^(?:for|on|in)\s+(?:each|every)\s+(?:` + cadenceWord + `\s+)*run,?\s*`)
	sentenceRE        = jsRE(`[^.!?]+(?:[.!?]+|$)\s*`)
)

// capitalizeFirst is text[0].toUpperCase() + text.slice(1) for the first
// UTF-16 unit. An astral first character is a lone surrogate to JavaScript,
// which toUpperCase leaves alone.
func capitalizeFirst(text string) string {
	r, size := utf8.DecodeRuneInString(text)
	if text == "" || r >= 0x10000 || r == utf8.RuneError {
		return text
	}
	upper := string(unicode.ToUpper(r))
	if r == 'ß' { // full case mapping, as String#toUpperCase does
		upper = "SS"
	}
	return upper + text[size:]
}

// TaskOnly is TaskOnlyInstructions's result: the kept text and, verbatim, what
// was removed.
type TaskOnly struct {
	Text    string   `json:"text"`
	Removed []string `json:"removed"`
}

// TaskOnlyInstructions drops scheduling and prompt-creation prose from a
// recurring request. A request that is nothing but such prose is kept whole
// (with nothing reported removed) rather than handed to the run empty; the
// workflow still describes the run on its own.
func TaskOnlyInstructions(instructions string) TaskOnly {
	removed := []string{}
	// Keep each sentence's trailing whitespace with it so the kept text retains
	// its own line breaks.
	sentences := sentenceRE.FindAllString(instructions, -1)
	if sentences == nil {
		sentences = []string{instructions}
	}
	var kept []string
	for _, sentence := range sentences {
		scheduling := false
		for _, p := range schedulingSentence {
			if p.MatchString(sentence) {
				scheduling = true
				break
			}
		}
		if scheduling {
			removed = append(removed, jsstr.Trim(sentence))
			continue
		}
		leading := sentence
		if len(kept) == 0 {
			leading = strings.TrimLeftFunc(sentence, jsstr.IsSpace)
		}
		framing := ""
		if m := leadingFraming.FindStringSubmatch(leading); m != nil {
			framing = m[1]
		} else if m := leadingRunFraming.FindString(leading); m != "" {
			framing = m
		}
		if framing != "" && jsstr.Trim(framing) != "" {
			removed = append(removed, jsstr.Trim(framing))
			kept = append(kept, capitalizeFirst(leading[len(framing):]))
			continue
		}
		kept = append(kept, sentence)
	}
	text := jsstr.Trim(strings.Join(kept, ""))
	if text == "" {
		return TaskOnly{Text: instructions, Removed: []string{}}
	}
	return TaskOnly{Text: text, Removed: removed}
}

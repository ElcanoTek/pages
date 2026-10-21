// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package agentguide is the agent guide behind the get_guide MCP tool.
//
// Pages ships its own instructions for agents: docs/AGENT_WORKFLOW.md (how to
// carry a request end to end) plus the reference guides it links to. They are
// embedded in each release, so the guide an agent reads always matches the
// tools that server actually runs.
//
// Only the fixed topics below are servable. A caller names a topic and a
// section slug, never a path, so nothing else is reachable. Documents are
// split on their "## " headings (outside fenced code), read once, and kept in
// memory; a release replaces the process, so there is nothing to reload.
package agentguide

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"sync"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/apierror"
)

// ProductVersion is the Pages version stamped on every guide result (the JS
// read package.json's "version"). It must look like N.N.N.
var ProductVersion = "0.1.0"

// TopicNames is the order of the index: the workflow first, then the
// references it links to. Every file here must be tenant-neutral (see the
// tests).
var TopicNames = []string{"workflow", "authoring", "templates", "data_updates", "motion"}

// Topics maps each topic to its file under docs/.
var Topics = map[string]string{
	"workflow":     "AGENT_WORKFLOW.md",
	"authoring":    "AUTHORING.md",
	"templates":    "TEMPLATES.md",
	"data_updates": "DATA_UPDATES.md",
	"motion":       "MOTION.md",
}

// SectionRE is the shape of a section slug.
var SectionRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Section is one "## " section of a document.
type Section struct {
	Slug  string
	Title string
	// Text runs from the heading line through the line before the next one.
	Text string
}

// Doc is a parsed guide document.
type Doc struct {
	Title    string
	Intro    string
	Sections []Section
}

var (
	slugDrop  = regexp.MustCompile("[`*_\"'“”‘’]")
	slugNonAN = regexp.MustCompile(`[^a-z0-9]+`)
	slugTrim  = regexp.MustCompile(`^-+|-+$`)
	fenceRE   = regexp.MustCompile("^\\s*(```|~~~)")
)

// Slugify turns a heading into its section slug.
func Slugify(title string) string {
	s := strings.ToLower(title)
	s = slugDrop.ReplaceAllString(s, "")
	s = slugNonAN.ReplaceAllString(s, "-")
	return slugTrim.ReplaceAllString(s, "")
}

// ParseDoc splits markdown into its title, introduction and sections. A
// section is its "## " heading through the line before the next one, so "###"
// subsections stay inside their parent. Headings inside ``` or ~~~ fences are
// content, not breaks.
func ParseDoc(markdown string) Doc {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	var doc Doc
	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			doc.Title = strings.TrimSpace(line[2:])
			break
		}
	}
	type building struct {
		slug, title string
		lines       []string
	}
	var intro []string
	var sections []*building
	var current *building
	fenced := false
	taken := func(slug string) bool {
		return slices.ContainsFunc(sections, func(s *building) bool { return s.slug == slug })
	}
	for _, line := range lines {
		if fenceRE.MatchString(line) {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "## ") {
			heading := strings.TrimSpace(line[3:])
			slug := Slugify(heading)
			if slug == "" {
				slug = fmt.Sprintf("section-%d", len(sections)+1)
			}
			for n := 2; taken(slug); n++ {
				slug = fmt.Sprintf("%s-%d", Slugify(heading), n)
			}
			current = &building{slug: slug, title: heading, lines: []string{line}}
			sections = append(sections, current)
			continue
		}
		if current != nil {
			current.lines = append(current.lines, line)
		} else {
			intro = append(intro, line)
		}
	}
	doc.Intro = strings.TrimSpace(strings.Join(intro, "\n"))
	for _, s := range sections {
		doc.Sections = append(doc.Sections, Section{Slug: s.slug, Title: s.title, Text: strings.TrimSpace(strings.Join(s.lines, "\n"))})
	}
	return doc
}

var (
	loadOnce sync.Once
	loaded   map[string]Doc
	errLoad  error
)

// docsFS is where the topics are read from; tests may swap it.
var docsFS = pages.Docs

func load() (map[string]Doc, error) {
	loadOnce.Do(func() {
		docs := map[string]Doc{}
		for _, topic := range TopicNames {
			b, err := fs.ReadFile(docsFS(), Topics[topic])
			if err != nil {
				errLoad = fmt.Errorf("agent guide: read %s: %w", Topics[topic], err)
				return
			}
			docs[topic] = ParseDoc(string(b))
		}
		loaded = docs
	})
	return loaded, errLoad
}

// Entry is one readable next step in a Result.
type Entry struct {
	Topic   string  `json:"topic"`
	Section *string `json:"section"`
	Title   string  `json:"title"`
	Bytes   int     `json:"bytes"`
}

// Result is the structured get_guide result. Field order is the wire order.
type Result struct {
	ProductVersion string  `json:"product_version"`
	Topic          *string `json:"topic"`
	Section        *string `json:"section"`
	Title          string  `json:"title"`
	Content        string  `json:"content"`
	ContentSHA256  string  `json:"content_sha256"`
	Entries        []Entry `json:"entries"`
}

func ptr(s string) *string { return &s }

func result(topic, section *string, title, content string, entries []Entry) *Result {
	sum := sha256.Sum256([]byte(content))
	if entries == nil {
		entries = []Entry{}
	}
	return &Result{
		ProductVersion: ProductVersion,
		Topic:          topic,
		Section:        section,
		Title:          title,
		Content:        content,
		ContentSHA256:  hex.EncodeToString(sum[:]),
		Entries:        entries,
	}
}

func index(docs map[string]Doc) *Result {
	entries := make([]Entry, 0, len(TopicNames))
	for _, topic := range TopicNames {
		d := docs[topic]
		n := len(d.Intro)
		for _, s := range d.Sections {
			n += len(s.Text)
		}
		entries = append(entries, Entry{Topic: topic, Title: d.Title, Bytes: n})
	}
	lines := []string{
		"# Pages agent guide",
		"",
		"Start with the workflow. Read the section for the step you are on; the other topics are the reference it links to.",
		"Links to AGENT_WORKFLOW.md, AUTHORING.md, TEMPLATES.md, DATA_UPDATES.md and MOTION.md are the topics workflow, authoring, templates, data_updates and motion.",
		"",
	}
	for _, e := range entries {
		lines = append(lines, "- `"+e.Topic+"`: "+e.Title)
	}
	return result(nil, nil, "Pages agent guide", strings.Join(lines, "\n"), entries)
}

func sectionEntries(topic string, sections []Section, skip int) []Entry {
	entries := []Entry{}
	for i, s := range sections {
		if i == skip {
			continue
		}
		entries = append(entries, Entry{Topic: topic, Section: ptr(s.Slug), Title: s.Title, Bytes: len(s.Text)})
	}
	return entries
}

func topicOverview(topic string, d Doc) *Result {
	entries := sectionEntries(topic, d.Sections, -1)
	lines := []string{d.Intro, "", "Sections:"}
	for _, e := range entries {
		lines = append(lines, "- `"+*e.Section+"`: "+e.Title)
	}
	return result(ptr(topic), nil, d.Title, strings.Join(lines, "\n"), entries)
}

// sectionNotFoundDetails is the details object of guide_section_not_found.
type sectionNotFoundDetails struct {
	Topic     string   `json:"topic"`
	Available []string `json:"available"`
}

// topicsDetails is the details object of guide_topic_required.
type topicsDetails struct {
	Topics []string `json:"topics"`
}

func sectionOf(topic string, d Doc, slug string) (*Result, error) {
	at := slices.IndexFunc(d.Sections, func(s Section) bool { return s.Slug == slug })
	if at < 0 {
		available := make([]string, len(d.Sections))
		for i, s := range d.Sections {
			available[i] = s.Slug
		}
		return nil, apierror.NotFound(fmt.Sprintf("no section %q in topic %s", slug, topic), "guide_section_not_found",
			sectionNotFoundDetails{Topic: topic, Available: available})
	}
	found := d.Sections[at]
	return result(ptr(topic), ptr(found.Slug), found.Title, found.Text, sectionEntries(topic, d.Sections, at)), nil
}

// Get answers get_guide. No topic: the index; a topic: its introduction and
// section list; a topic and section: that section's text. Every result lists
// what can be read next, so an agent never has to guess a slug. nil means the
// argument was omitted.
//
// The tool's input schema admits only TopicNames; an unknown topic (reachable
// only by bypassing it) is a 400 guide_topic_unknown rather than the JS
// TypeError.
func Get(topic, section *string) (*Result, error) {
	if section != nil && topic == nil {
		return nil, apierror.BadRequest("a section needs its topic", "guide_topic_required",
			topicsDetails{Topics: slices.Clone(TopicNames)})
	}
	docs, err := load()
	if err != nil {
		return nil, err
	}
	if topic == nil {
		return index(docs), nil
	}
	d, ok := docs[*topic]
	if !ok {
		return nil, apierror.BadRequest("unknown guide topic", "guide_topic_unknown",
			topicsDetails{Topics: slices.Clone(TopicNames)})
	}
	if section == nil {
		return topicOverview(*topic, d), nil
	}
	return sectionOf(*topic, d, *section)
}

// Render is the text an agent reads. Hosts that pass only text content to the
// model get the Markdown itself rather than a JSON string of it; the
// structured result carries the same content plus its metadata. A section's
// text also names the topic's other sections, so a text-only host sees
// everything the structured entries offer.
func Render(out *Result) string {
	var parts []string
	if out.Topic != nil && *out.Topic != "" {
		parts = append(parts, *out.Topic)
	}
	if out.Section != nil && *out.Section != "" {
		parts = append(parts, *out.Section)
	}
	where := strings.Join(parts, " / ")
	if where == "" {
		where = "index"
	}
	more := ""
	if out.Section != nil && *out.Section != "" {
		slugs := make([]string, len(out.Entries))
		for i, e := range out.Entries {
			if e.Section != nil {
				slugs[i] = *e.Section
			}
		}
		more = "\nOther sections in " + *out.Topic + ": " + strings.Join(slugs, ", ")
	}
	return out.Content + "\n\n---\nget_guide · " + where + " · Pages " + out.ProductVersion + " · sha256 " + out.ContentSHA256[:12] + more
}

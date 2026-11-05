package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"codedeep/internal/llm"
	"codedeep/internal/util"
)

// Search returns sessions whose title or message text contains every term in
// the query. Title matches rank ahead of body matches; both stay newest-first.
// An empty query behaves like List.
func (s *Store) Search(query string) ([]Meta, error) {
	metas, err := s.List()
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" {
		return metas, nil
	}
	terms := strings.Fields(query)
	type hit struct {
		meta    Meta
		title   bool
		snippet string
	}
	hits := make([]hit, 0, len(metas))
	for _, m := range metas {
		sess, err := s.Load(m.ID)
		if err != nil {
			continue
		}
		title := strings.ToLower(sess.Title)
		titleMatch := containsAll(title, terms)
		body := searchText(sess.Messages)
		if !titleMatch && !containsAll(strings.ToLower(body), terms) {
			continue
		}
		m.Snippet = snippetFor(body, terms)
		hits = append(hits, hit{meta: m, title: titleMatch, snippet: m.Snippet})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].title && !hits[j].title })
	out := make([]Meta, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.meta)
	}
	return out, nil
}

func containsAll(text string, terms []string) bool {
	for _, term := range terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func searchText(messages []llm.Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
		for _, tc := range m.ToolCalls {
			b.WriteString(tc.Function.Name)
			b.WriteByte(' ')
			b.WriteString(tc.Function.Arguments)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func snippetFor(text string, terms []string) string {
	flat := strings.Join(strings.Fields(text), " ")
	lower := strings.ToLower(flat)
	idx := -1
	for _, term := range terms {
		if i := strings.Index(lower, term); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	if idx < 0 {
		return ""
	}
	start := idx - 60
	if start < 0 {
		start = 0
	}
	end := idx + 120
	if end > len(flat) {
		end = len(flat)
	}
	for start > 0 && flat[start]&0xC0 == 0x80 {
		start--
	}
	for end < len(flat) && flat[end]&0xC0 == 0x80 {
		end++
	}
	snippet := strings.TrimSpace(flat[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(flat) {
		snippet += "…"
	}
	return util.Truncate(snippet, 180)
}

// Resolve finds a session by exact id, exact title (case-insensitive), unique
// id prefix, or unique title substring. Ambiguous references return an error
// that lists the candidates.
func (s *Store) Resolve(ref string) (*Session, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("empty session reference")
	}
	if sess, err := s.Load(ref); err == nil {
		return sess, nil
	}
	metas, err := s.List()
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(ref)

	var exact []Meta
	for _, m := range metas {
		if strings.EqualFold(m.Title, ref) {
			exact = append(exact, m)
		}
	}
	if len(exact) == 1 {
		return s.Load(exact[0].ID)
	}
	if len(exact) > 1 {
		return nil, ambiguousError(ref, exact)
	}

	var prefix []Meta
	for _, m := range metas {
		if strings.HasPrefix(strings.ToLower(m.ID), lower) {
			prefix = append(prefix, m)
		}
	}
	if len(prefix) == 1 {
		return s.Load(prefix[0].ID)
	}
	if len(prefix) > 1 {
		return nil, ambiguousError(ref, prefix)
	}

	var partial []Meta
	for _, m := range metas {
		if strings.Contains(strings.ToLower(m.Title), lower) {
			partial = append(partial, m)
		}
	}
	if len(partial) == 1 {
		return s.Load(partial[0].ID)
	}
	if len(partial) > 1 {
		return nil, ambiguousError(ref, partial)
	}
	return nil, os.ErrNotExist
}

func ambiguousError(ref string, metas []Meta) error {
	names := make([]string, 0, len(metas))
	for i, m := range metas {
		if i == 5 {
			names = append(names, "…")
			break
		}
		names = append(names, fmt.Sprintf("%s (%s)", m.ID, m.Title))
	}
	return fmt.Errorf("session %q is ambiguous: %s", ref, strings.Join(names, ", "))
}

// Export renders a session as markdown or JSON. It returns the payload, its
// content type, and a filename suitable for download.
func Export(sess *Session, format string) ([]byte, string, string, error) {
	if sess == nil {
		return nil, "", "", errors.New("nil session")
	}
	format = strings.ToLower(strings.TrimSpace(format))
	switch format {
	case "", "md", "markdown":
		return []byte(exportMarkdown(sess)), "text/markdown; charset=utf-8",
			exportFilename(sess, "md"), nil
	case "json":
		data, err := json.MarshalIndent(sess, "", "  ")
		if err != nil {
			return nil, "", "", err
		}
		return append(data, '\n'), "application/json; charset=utf-8",
			exportFilename(sess, "json"), nil
	default:
		return nil, "", "", fmt.Errorf("unsupported export format %q (use md or json)", format)
	}
}

func exportMarkdown(sess *Session) string {
	var b strings.Builder
	title := strings.TrimSpace(sess.Title)
	if title == "" {
		title = "Untitled session"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "- **Session:** `%s`\n", sess.ID)
	fmt.Fprintf(&b, "- **Model:** %s\n", sess.Model)
	fmt.Fprintf(&b, "- **Workspace:** `%s`\n", sess.Cwd)
	if !sess.Created.IsZero() {
		fmt.Fprintf(&b, "- **Created:** %s\n", sess.Created.Format("2006-01-02 15:04:05 MST"))
	}
	if !sess.Updated.IsZero() {
		fmt.Fprintf(&b, "- **Updated:** %s\n", sess.Updated.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Fprintf(&b, "- **Turns:** %d\n", sess.Turns)
	fmt.Fprintf(&b, "- **Tokens:** %d (prompt %d, completion %d)\n", sess.Usage.TotalTokens,
		sess.Usage.PromptTokens, sess.Usage.CompletionTokens)
	b.WriteString("\n---\n\n")

	for _, m := range sess.Messages {
		switch m.Role {
		case "user":
			b.WriteString("## User\n\n")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n\n")
			for i, img := range m.Images {
				mime := strings.TrimSpace(img.MIME)
				if mime == "" {
					mime = "image/png"
				}
				fmt.Fprintf(&b, "![attached image %d](data:%s;base64,%s)\n\n", i+1, mime, strings.TrimSpace(img.Data))
			}
		case "assistant":
			b.WriteString("## Assistant\n\n")
			if text := strings.TrimSpace(m.Content); text != "" {
				b.WriteString(text)
				b.WriteString("\n\n")
			}
			if reasoning := strings.TrimSpace(m.Reasoning); reasoning != "" {
				b.WriteString("<details><summary>Reasoning</summary>\n\n")
				b.WriteString(reasoning)
				b.WriteString("\n\n</details>\n\n")
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "**Tool call:** `%s`\n\n", tc.Function.Name)
				b.WriteString("```json\n")
				b.WriteString(prettyJSON(tc.Function.Arguments))
				b.WriteString("\n```\n\n")
			}
		case "tool":
			b.WriteString("**Tool result**\n\n```text\n")
			b.WriteString(strings.TrimSpace(m.Content))
			b.WriteString("\n```\n\n")
		default:
			if text := strings.TrimSpace(m.Content); text != "" {
				fmt.Fprintf(&b, "**%s**\n\n%s\n\n", m.Role, text)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func prettyJSON(raw string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil || buf.Len() == 0 {
		return strings.TrimSpace(raw)
	}
	return buf.String()
}

func exportFilename(sess *Session, ext string) string {
	slug := slugify(sess.Title)
	if slug == "" {
		slug = "session"
	}
	return fmt.Sprintf("codedeep-%s-%s.%s", slug, sess.ID, ext)
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == ' ' || r == '-' || r == '_':
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

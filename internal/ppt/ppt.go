package ppt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"unicode/utf8"
)

const SchemaVersion = "ppt-outline/v1"

var layouts = map[string]bool{
	"title": true, "bullets": true, "two-column": true, "quote": true,
	"timeline": true, "comparison": true, "closing": true,
}

type Deck struct {
	SchemaVersion string  `json:"schema_version"`
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle,omitempty"`
	Audience      string  `json:"audience,omitempty"`
	Language      string  `json:"language,omitempty"`
	Theme         Theme   `json:"theme"`
	Slides        []Slide `json:"slides"`
}

type Theme struct {
	Name        string `json:"name,omitempty"`
	Tone        string `json:"tone,omitempty"`
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

type Slide struct {
	ID           string   `json:"id"`
	Layout       string   `json:"layout"`
	Title        string   `json:"title"`
	Subtitle     string   `json:"subtitle,omitempty"`
	Bullets      []string `json:"bullets,omitempty"`
	LeftColumn   *Column  `json:"left_column,omitempty"`
	RightColumn  *Column  `json:"right_column,omitempty"`
	SpeakerNotes string   `json:"speaker_notes,omitempty"`
}

type Column struct {
	Heading string   `json:"heading,omitempty"`
	Bullets []string `json:"bullets,omitempty"`
}

func Decode(data []byte) (Deck, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var deck Deck
	if err := decoder.Decode(&deck); err != nil {
		return Deck{}, fmt.Errorf("PPT JSON 格式无效: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Deck{}, fmt.Errorf("PPT JSON 只能包含一个对象")
		}
		return Deck{}, fmt.Errorf("PPT JSON 格式无效: %w", err)
	}
	if err := Validate(deck); err != nil {
		return Deck{}, err
	}
	return deck, nil
}

func Validate(deck Deck) error {
	if deck.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version 必须为 %s", SchemaVersion)
	}
	if err := requiredText("title", deck.Title, 120); err != nil {
		return err
	}
	if err := optionalText("subtitle", deck.Subtitle, 240); err != nil { return err }
	if err := optionalText("audience", deck.Audience, 160); err != nil { return err }
	if err := optionalText("language", deck.Language, 40); err != nil { return err }
	if err := optionalText("theme.name", deck.Theme.Name, 120); err != nil { return err }
	if err := optionalText("theme.tone", deck.Theme.Tone, 80); err != nil { return err }
	if err := optionalText("theme.aspect_ratio", deck.Theme.AspectRatio, 20); err != nil { return err }
	if len(deck.Slides) < 1 || len(deck.Slides) > 30 {
		return fmt.Errorf("slides 数量必须介于 1 到 30")
	}
	seen := make(map[string]bool, len(deck.Slides))
	for index, slide := range deck.Slides {
		prefix := fmt.Sprintf("slides[%d]", index)
		if err := requiredText(prefix+".id", slide.ID, 80); err != nil {
			return err
		}
		if seen[slide.ID] {
			return fmt.Errorf("%s.id 必须唯一", prefix)
		}
		seen[slide.ID] = true
		if !layouts[slide.Layout] {
			return fmt.Errorf("%s.layout 不受支持", prefix)
		}
		if err := requiredText(prefix+".title", slide.Title, 160); err != nil {
			return err
		}
		if err := optionalText(prefix+".subtitle", slide.Subtitle, 240); err != nil {
			return err
		}
		if err := validateBullets(prefix+".bullets", slide.Bullets); err != nil {
			return err
		}
		if err := validateColumn(prefix+".left_column", slide.LeftColumn); err != nil {
			return err
		}
		if err := validateColumn(prefix+".right_column", slide.RightColumn); err != nil {
			return err
		}
		if err := optionalText(prefix+".speaker_notes", slide.SpeakerNotes, 2000); err != nil {
			return err
		}
		if slide.Layout == "two-column" || slide.Layout == "comparison" {
			if slide.LeftColumn == nil || slide.RightColumn == nil {
				return fmt.Errorf("%s.layout 需要 left_column 和 right_column", prefix)
			}
		}
	}
	return nil
}

func requiredText(name, value string, max int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s 不能为空", name)
	}
	return optionalText(name, value, max)
}

func optionalText(name, value string, max int) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || utf8.RuneCountInString(value) > max {
		return fmt.Errorf("%s 内容无效或过长", name)
	}
	return nil
}

func validateBullets(name string, values []string) error {
	if len(values) > 8 {
		return fmt.Errorf("%s 最多 8 项", name)
	}
	for _, value := range values {
		if err := requiredText(name, value, 300); err != nil {
			return err
		}
	}
	return nil
}

func validateColumn(name string, column *Column) error {
	if column == nil {
		return nil
	}
	if err := optionalText(name+".heading", column.Heading, 160); err != nil {
		return err
	}
	return validateBullets(name+".bullets", column.Bullets)
}

func Markdown(deck Deck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", deck.Title)
	if deck.Subtitle != "" { fmt.Fprintf(&b, "%s\n\n", deck.Subtitle) }
	if deck.Audience != "" { fmt.Fprintf(&b, "- 受众：%s\n", deck.Audience) }
	if deck.Theme.Name != "" { fmt.Fprintf(&b, "- 主题：%s\n", deck.Theme.Name) }
	b.WriteString("\n")
	for index, slide := range deck.Slides {
		fmt.Fprintf(&b, "## %d. %s\n\n", index+1, slide.Title)
		if slide.Subtitle != "" { fmt.Fprintf(&b, "%s\n\n", slide.Subtitle) }
		for _, bullet := range slide.Bullets { fmt.Fprintf(&b, "- %s\n", bullet) }
		for _, column := range []*Column{slide.LeftColumn, slide.RightColumn} {
			if column == nil { continue }
			if column.Heading != "" { fmt.Fprintf(&b, "\n### %s\n", column.Heading) }
			for _, bullet := range column.Bullets { fmt.Fprintf(&b, "- %s\n", bullet) }
		}
		if slide.SpeakerNotes != "" { fmt.Fprintf(&b, "\n> 备注：%s\n", strings.ReplaceAll(slide.SpeakerNotes, "\n", "\n> ")) }
		b.WriteString("\n")
	}
	return b.String()
}

var htmlTemplate = template.Must(template.New("ppt").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>{{.Title}}</title><style>body{font-family:system-ui,sans-serif;background:#f5f7fb;color:#172033;margin:0}.slide{box-sizing:border-box;max-width:1120px;min-height:630px;margin:24px auto;padding:64px;background:#fff;border:1px solid #d8dee9;border-radius:20px}.meta{color:#5b667a}.columns{display:grid;grid-template-columns:1fr 1fr;gap:28px}@media print{.slide{margin:0;min-height:100vh;border:0;border-radius:0;page-break-after:always}}</style></head><body>{{range .Slides}}<section class="slide"><p class="meta">{{.Layout}}</p><h1>{{.Title}}</h1>{{if .Subtitle}}<h2>{{.Subtitle}}</h2>{{end}}{{if .Bullets}}<ul>{{range .Bullets}}<li>{{.}}</li>{{end}}</ul>{{end}}{{if or .LeftColumn .RightColumn}}<div class="columns">{{with .LeftColumn}}<div><h2>{{.Heading}}</h2><ul>{{range .Bullets}}<li>{{.}}</li>{{end}}</ul></div>{{end}}{{with .RightColumn}}<div><h2>{{.Heading}}</h2><ul>{{range .Bullets}}<li>{{.}}</li>{{end}}</ul></div>{{end}}</div>{{end}}{{if .SpeakerNotes}}<p class="meta">备注：{{.SpeakerNotes}}</p>{{end}}</section>{{end}}</body></html>`))

func HTML(deck Deck) (string, error) {
	var b bytes.Buffer
	if err := htmlTemplate.Execute(&b, deck); err != nil { return "", err }
	return b.String(), nil
}

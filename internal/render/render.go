// Package render turns validated JSON payloads into PDF documents.
package render

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/go-pdf/fpdf"

	"github.com/tok1e1/doc-processor/internal/domain"
)

//go:embed fonts/*.ttf
var fonts embed.FS

const fontFamily = "DejaVu"

// Template describes a single document type.
type Template interface {
	Name() string
	// Validate checks that the payload can be rendered. It must be cheap: the API calls it synchronously.
	Validate(payload json.RawMessage) error
	Render(doc *fpdf.Fpdf, payload json.RawMessage) error
}

type Registry struct {
	templates map[string]Template
	regular   []byte
	bold      []byte
}

func NewRegistry(templates ...Template) (*Registry, error) {
	regular, err := fonts.ReadFile("fonts/DejaVuSans.ttf")
	if err != nil {
		return nil, fmt.Errorf("load regular font: %w", err)
	}
	bold, err := fonts.ReadFile("fonts/DejaVuSans-Bold.ttf")
	if err != nil {
		return nil, fmt.Errorf("load bold font: %w", err)
	}

	r := &Registry{templates: make(map[string]Template, len(templates)), regular: regular, bold: bold}
	for _, t := range templates {
		if _, dup := r.templates[t.Name()]; dup {
			return nil, fmt.Errorf("template %q registered twice", t.Name())
		}
		r.templates[t.Name()] = t
	}
	return r, nil
}

// Default returns a registry with all built-in templates.
func Default() (*Registry, error) {
	return NewRegistry(Invoice{}, OfferLetter{})
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.templates))
	for n := range r.templates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (r *Registry) Validate(name string, payload json.RawMessage) error {
	t, ok := r.templates[name]
	if !ok {
		return domain.ErrUnknownTemplate
	}
	return t.Validate(payload)
}

// Render writes the PDF to w. Rendering itself is CPU-bound and not interruptible,
// so ctx is only checked before the expensive steps.
func (r *Registry) Render(ctx context.Context, name string, payload json.RawMessage, w io.Writer) error {
	t, ok := r.templates[name]
	if !ok {
		return domain.ErrUnknownTemplate
	}
	if err := t.Validate(payload); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	doc := fpdf.New("P", "mm", "A4", "")
	doc.SetCreator("doc-processor", true)
	doc.AddUTF8FontFromBytes(fontFamily, "", r.regular)
	doc.AddUTF8FontFromBytes(fontFamily, "B", r.bold)
	doc.SetMargins(20, 20, 20)
	doc.SetAutoPageBreak(true, 20)
	doc.AddPage()

	if err := t.Render(doc, payload); err != nil {
		return err
	}
	if err := doc.Error(); err != nil {
		return fmt.Errorf("build pdf: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Buffer the output so a failed render never leaves a half-written file behind.
	var buf bytes.Buffer
	if err := doc.Output(&buf); err != nil {
		return fmt.Errorf("write pdf: %w", err)
	}
	_, err := buf.WriteTo(w)
	return err
}

func decodeStrict(payload json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &domain.ValidationError{Field: "data", Reason: err.Error()}
	}
	return nil
}

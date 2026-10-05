package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tok1e1/doc-processor/internal/domain"
)

const validInvoice = `{
	"number": "INV-2026-0042",
	"date": "2026-10-01",
	"due_date": "2026-10-15",
	"currency": "RUB",
	"vat_rate": 20,
	"seller": {"name": "ООО «Ромашка»", "tax_id": "7701234567", "address": "Москва, ул. Ленина, 1"},
	"buyer": {"name": "Acme Corp"},
	"items": [
		{"name": "Backend development", "quantity": 40, "unit_price": 350000},
		{"name": "Code review", "quantity": 3, "unit_price": 199999}
	]
}`

const validOffer = `{
	"company": "Acme Corp",
	"employee": {"full_name": "Иван Петров", "email": "ivan@example.com"},
	"position": "Go Developer",
	"department": "Platform",
	"start_date": "2026-11-01",
	"salary": {"amount": 30000000, "currency": "RUB"},
	"probation_months": 3,
	"signer": "Jane Doe, CTO"
}`

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// patch replaces a top-level field of a JSON object; value == nil deletes the field.
func patch(t *testing.T, doc, field string, value any) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(m, field)
	} else {
		m[field] = value
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestValidate(t *testing.T) {
	r := newRegistry(t)

	tests := []struct {
		name      string
		template  string
		payload   json.RawMessage
		wantField string
	}{
		{"valid invoice", "invoice", json.RawMessage(validInvoice), ""},
		{"valid offer", "offer_letter", json.RawMessage(validOffer), ""},
		{"invoice without number", "invoice", patch(t, validInvoice, "number", nil), "number"},
		{"invoice bad date", "invoice", patch(t, validInvoice, "date", "01.10.2026"), "date"},
		{"invoice due before date", "invoice", patch(t, validInvoice, "due_date", "2026-09-01"), "due_date"},
		{"invoice lowercase currency", "invoice", patch(t, validInvoice, "currency", "rub"), "currency"},
		{"invoice vat out of range", "invoice", patch(t, validInvoice, "vat_rate", 120), "vat_rate"},
		{"invoice no items", "invoice", patch(t, validInvoice, "items", []any{}), "items"},
		{"invoice zero quantity", "invoice", patch(t, validInvoice, "items", []any{
			map[string]any{"name": "x", "quantity": 0, "unit_price": 1},
		}), "items[0].quantity"},
		{"invoice unknown field", "invoice", patch(t, validInvoice, "discount", 10), "data"},
		{"offer bad email", "offer_letter", patch(t, validOffer, "employee", map[string]any{
			"full_name": "Ivan", "email": "not-an-email",
		}), "employee.email"},
		{"offer long probation", "offer_letter", patch(t, validOffer, "probation_months", 12), "probation_months"},
		{"offer without signer", "offer_letter", patch(t, validOffer, "signer", ""), "signer"},
		{"not an object", "invoice", json.RawMessage(`[1,2,3]`), "data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.Validate(tt.template, tt.payload)
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var verr *domain.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if verr.Field != tt.wantField {
				t.Fatalf("want field %q, got %q (%s)", tt.wantField, verr.Field, verr.Reason)
			}
		})
	}
}

func TestValidateUnknownTemplate(t *testing.T) {
	err := newRegistry(t).Validate("passport", json.RawMessage(`{}`))
	if !errors.Is(err, domain.ErrUnknownTemplate) {
		t.Fatalf("want ErrUnknownTemplate, got %v", err)
	}
}

func TestRenderProducesPDF(t *testing.T) {
	r := newRegistry(t)
	for name, payload := range map[string]string{"invoice": validInvoice, "offer_letter": validOffer} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := r.Render(context.Background(), name, json.RawMessage(payload), &buf); err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) {
				t.Fatalf("output is not a PDF: %q", buf.Bytes()[:min(16, buf.Len())])
			}
			if buf.Len() < 1024 {
				t.Fatalf("suspiciously small PDF: %d bytes", buf.Len())
			}
		})
	}
}

func TestRenderRespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	err := newRegistry(t).Render(ctx, "invoice", json.RawMessage(validInvoice), &buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("nothing must be written on cancellation")
	}
}

func TestInvoiceTotals(t *testing.T) {
	tests := []struct {
		name                   string
		vat                    int64
		items                  []InvoiceItem
		subtotal, tax, grandTo int64
	}{
		{"no vat", 0, []InvoiceItem{{Quantity: 2, UnitPrice: 1050}}, 2100, 0, 2100},
		{"20 percent", 20, []InvoiceItem{{Quantity: 1, UnitPrice: 10000}}, 10000, 2000, 12000},
		{"rounds down below half", 20, []InvoiceItem{{Quantity: 1, UnitPrice: 1}, {Quantity: 1, UnitPrice: 1}}, 2, 0, 2},
		{"rounds .5 up", 10, []InvoiceItem{{Quantity: 1, UnitPrice: 5}}, 5, 1, 6},
		{"large amounts", 20, []InvoiceItem{{Quantity: maxQuantity, UnitPrice: maxUnitPrice}}, 1e16, 2e15, 1.2e16},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, v, total := InvoiceData{VATRate: tt.vat, Items: tt.items}.Totals()
			if s != tt.subtotal || v != tt.tax || total != tt.grandTo {
				t.Fatalf("got (%d, %d, %d), want (%d, %d, %d)", s, v, total, tt.subtotal, tt.tax, tt.grandTo)
			}
		})
	}
}

func TestFormatMoney(t *testing.T) {
	tests := map[int64]string{
		0:          "0.00",
		5:          "0.05",
		100:        "1.00",
		123456789:  "1 234 567.89",
		-150050:    "-1 500.50",
		1000000000: "10 000 000.00",
	}
	for in, want := range tests {
		if got := formatMoney(in); got != want {
			t.Errorf("formatMoney(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	_, err := NewRegistry(Invoice{}, Invoice{})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

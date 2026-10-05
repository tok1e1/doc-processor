package render

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/go-pdf/fpdf"

	"github.com/tok1e1/doc-processor/internal/domain"
)

// Limits keep every intermediate sum well inside int64.
const (
	maxInvoiceItems = 500
	maxQuantity     = 100_000
	maxUnitPrice    = 100_000_000_000 // minor units
)

type Party struct {
	Name    string `json:"name"`
	TaxID   string `json:"tax_id,omitempty"`
	Address string `json:"address,omitempty"`
}

type InvoiceItem struct {
	Name      string `json:"name"`
	Quantity  int64  `json:"quantity"`
	UnitPrice int64  `json:"unit_price"` // minor units
}

type InvoiceData struct {
	Number   string        `json:"number"`
	Date     string        `json:"date"`
	DueDate  string        `json:"due_date,omitempty"`
	Currency string        `json:"currency"`
	VATRate  int64         `json:"vat_rate"` // percent
	Seller   Party         `json:"seller"`
	Buyer    Party         `json:"buyer"`
	Items    []InvoiceItem `json:"items"`
}

type Invoice struct{}

func (Invoice) Name() string { return "invoice" }

func (Invoice) Validate(payload json.RawMessage) error {
	_, err := parseInvoice(payload)
	return err
}

func parseInvoice(payload json.RawMessage) (InvoiceData, error) {
	var d InvoiceData
	if err := decodeStrict(payload, &d); err != nil {
		return d, err
	}
	if err := required("number", d.Number); err != nil {
		return d, err
	}
	date, err := parseDate("date", d.Date)
	if err != nil {
		return d, err
	}
	if d.DueDate != "" {
		due, err := parseDate("due_date", d.DueDate)
		if err != nil {
			return d, err
		}
		if due.Before(date) {
			return d, &domain.ValidationError{Field: "due_date", Reason: "must not be before date"}
		}
	}
	if err := validCurrency("currency", d.Currency); err != nil {
		return d, err
	}
	if d.VATRate < 0 || d.VATRate > 100 {
		return d, &domain.ValidationError{Field: "vat_rate", Reason: "must be between 0 and 100"}
	}
	if err := required("seller.name", d.Seller.Name); err != nil {
		return d, err
	}
	if err := required("buyer.name", d.Buyer.Name); err != nil {
		return d, err
	}
	if len(d.Items) == 0 || len(d.Items) > maxInvoiceItems {
		return d, &domain.ValidationError{Field: "items", Reason: fmt.Sprintf("must contain 1..%d items", maxInvoiceItems)}
	}
	for i, it := range d.Items {
		field := "items[" + strconv.Itoa(i) + "]"
		if err := required(field+".name", it.Name); err != nil {
			return d, err
		}
		if it.Quantity < 1 || it.Quantity > maxQuantity {
			return d, &domain.ValidationError{Field: field + ".quantity", Reason: fmt.Sprintf("must be in 1..%d", maxQuantity)}
		}
		if it.UnitPrice < 0 || it.UnitPrice > maxUnitPrice {
			return d, &domain.ValidationError{Field: field + ".unit_price", Reason: "is out of range"}
		}
	}
	return d, nil
}

// Totals returns subtotal, VAT and grand total in minor units. VAT is rounded half up.
func (d InvoiceData) Totals() (subtotal, vat, total int64) {
	for _, it := range d.Items {
		subtotal += it.Quantity * it.UnitPrice
	}
	vat = subtotal/100*d.VATRate + (subtotal%100*d.VATRate+50)/100
	return subtotal, vat, subtotal + vat
}

func (Invoice) Render(doc *fpdf.Fpdf, payload json.RawMessage) error {
	d, err := parseInvoice(payload)
	if err != nil {
		return err
	}

	doc.SetFont(fontFamily, "B", 18)
	doc.CellFormat(0, 10, "Invoice № "+d.Number, "", 1, "L", false, 0, "")
	doc.SetFont(fontFamily, "", 10)
	doc.CellFormat(0, 6, "Date: "+d.Date, "", 1, "L", false, 0, "")
	if d.DueDate != "" {
		doc.CellFormat(0, 6, "Due date: "+d.DueDate, "", 1, "L", false, 0, "")
	}
	doc.Ln(4)

	party(doc, "Seller", d.Seller)
	party(doc, "Buyer", d.Buyer)
	doc.Ln(2)

	widths := []float64{10, 85, 20, 27, 28}
	header := []string{"#", "Description", "Qty", "Unit price", "Amount"}
	doc.SetFont(fontFamily, "B", 10)
	doc.SetFillColor(235, 238, 242)
	for i, h := range header {
		doc.CellFormat(widths[i], 8, h, "1", 0, "C", true, 0, "")
	}
	doc.Ln(-1)

	doc.SetFont(fontFamily, "", 10)
	for i, it := range d.Items {
		row := []string{
			strconv.Itoa(i + 1),
			it.Name,
			strconv.FormatInt(it.Quantity, 10),
			formatMoney(it.UnitPrice),
			formatMoney(it.Quantity * it.UnitPrice),
		}
		align := []string{"C", "L", "R", "R", "R"}
		for j, cell := range row {
			doc.CellFormat(widths[j], 7, truncate(doc, cell, widths[j]-2), "1", 0, align[j], false, 0, "")
		}
		doc.Ln(-1)
	}

	subtotal, vat, total := d.Totals()
	doc.Ln(2)
	totalRow(doc, "Subtotal", formatMoney(subtotal)+" "+d.Currency, false)
	totalRow(doc, "VAT "+strconv.FormatInt(d.VATRate, 10)+"%", formatMoney(vat)+" "+d.Currency, false)
	totalRow(doc, "Total", formatMoney(total)+" "+d.Currency, true)
	return nil
}

func party(doc *fpdf.Fpdf, title string, p Party) {
	doc.SetFont(fontFamily, "B", 11)
	doc.CellFormat(0, 6, title, "", 1, "L", false, 0, "")
	doc.SetFont(fontFamily, "", 10)
	doc.CellFormat(0, 5, p.Name, "", 1, "L", false, 0, "")
	if p.TaxID != "" {
		doc.CellFormat(0, 5, "Tax ID: "+p.TaxID, "", 1, "L", false, 0, "")
	}
	if p.Address != "" {
		doc.MultiCell(0, 5, p.Address, "", "L", false)
	}
	doc.Ln(2)
}

func totalRow(doc *fpdf.Fpdf, label, value string, bold bool) {
	style := ""
	if bold {
		style = "B"
	}
	doc.SetFont(fontFamily, style, 10)
	doc.CellFormat(90, 7, "", "", 0, "L", false, 0, "")
	doc.CellFormat(35, 7, label, "", 0, "L", false, 0, "")
	doc.CellFormat(45, 7, value, "", 1, "R", false, 0, "")
}

// truncate shortens s with an ellipsis so it fits into a table cell of the given width.
func truncate(doc *fpdf.Fpdf, s string, width float64) string {
	if doc.GetStringWidth(s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && doc.GetStringWidth(string(r)+"…") > width {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

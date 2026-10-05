package render

import (
	"encoding/json"
	"fmt"
	"net/mail"

	"github.com/go-pdf/fpdf"

	"github.com/tok1e1/doc-processor/internal/domain"
)

type Employee struct {
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

type Salary struct {
	Amount   int64  `json:"amount"` // minor units, gross per month
	Currency string `json:"currency"`
}

type OfferLetterData struct {
	Company         string   `json:"company"`
	Employee        Employee `json:"employee"`
	Position        string   `json:"position"`
	Department      string   `json:"department,omitempty"`
	StartDate       string   `json:"start_date"`
	Salary          Salary   `json:"salary"`
	ProbationMonths int      `json:"probation_months"`
	Signer          string   `json:"signer"`
}

type OfferLetter struct{}

func (OfferLetter) Name() string { return "offer_letter" }

func (OfferLetter) Validate(payload json.RawMessage) error {
	_, err := parseOfferLetter(payload)
	return err
}

func parseOfferLetter(payload json.RawMessage) (OfferLetterData, error) {
	var d OfferLetterData
	if err := decodeStrict(payload, &d); err != nil {
		return d, err
	}
	for _, f := range []struct{ name, value string }{
		{"company", d.Company},
		{"employee.full_name", d.Employee.FullName},
		{"position", d.Position},
		{"signer", d.Signer},
	} {
		if err := required(f.name, f.value); err != nil {
			return d, err
		}
	}
	if _, err := mail.ParseAddress(d.Employee.Email); err != nil {
		return d, &domain.ValidationError{Field: "employee.email", Reason: "must be a valid email"}
	}
	if _, err := parseDate("start_date", d.StartDate); err != nil {
		return d, err
	}
	if d.Salary.Amount <= 0 || d.Salary.Amount > maxUnitPrice {
		return d, &domain.ValidationError{Field: "salary.amount", Reason: "is out of range"}
	}
	if err := validCurrency("salary.currency", d.Salary.Currency); err != nil {
		return d, err
	}
	if d.ProbationMonths < 0 || d.ProbationMonths > 6 {
		return d, &domain.ValidationError{Field: "probation_months", Reason: "must be between 0 and 6"}
	}
	return d, nil
}

func (OfferLetter) Render(doc *fpdf.Fpdf, payload json.RawMessage) error {
	d, err := parseOfferLetter(payload)
	if err != nil {
		return err
	}

	doc.SetFont(fontFamily, "B", 16)
	doc.CellFormat(0, 10, d.Company, "", 1, "L", false, 0, "")
	doc.SetFont(fontFamily, "B", 13)
	doc.CellFormat(0, 9, "Job Offer", "", 1, "L", false, 0, "")
	doc.Ln(4)

	doc.SetFont(fontFamily, "", 11)
	doc.MultiCell(0, 6, fmt.Sprintf("Dear %s,", d.Employee.FullName), "", "L", false)
	doc.Ln(2)
	doc.MultiCell(0, 6, fmt.Sprintf(
		"We are pleased to offer you the position of %s at %s. "+
			"Please find the key terms of the offer below.", d.Position, d.Company), "", "L", false)
	doc.Ln(4)

	rows := [][2]string{
		{"Position", d.Position},
		{"Start date", d.StartDate},
		{"Gross salary", formatMoney(d.Salary.Amount) + " " + d.Salary.Currency + " / month"},
		{"Probation", probation(d.ProbationMonths)},
	}
	if d.Department != "" {
		rows = append(rows[:1], append([][2]string{{"Department", d.Department}}, rows[1:]...)...)
	}
	for _, r := range rows {
		doc.SetFont(fontFamily, "B", 11)
		doc.CellFormat(45, 8, r[0], "B", 0, "L", false, 0, "")
		doc.SetFont(fontFamily, "", 11)
		doc.CellFormat(0, 8, r[1], "B", 1, "L", false, 0, "")
	}

	doc.Ln(8)
	doc.MultiCell(0, 6, "We look forward to working with you.", "", "L", false)
	doc.Ln(10)
	doc.CellFormat(0, 6, "Sincerely,", "", 1, "L", false, 0, "")
	doc.SetFont(fontFamily, "B", 11)
	doc.CellFormat(0, 6, d.Signer, "", 1, "L", false, 0, "")
	return nil
}

func probation(months int) string {
	switch months {
	case 0:
		return "none"
	case 1:
		return "1 month"
	default:
		return fmt.Sprintf("%d months", months)
	}
}

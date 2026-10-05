package render

import (
	"strconv"
	"strings"
	"time"

	"github.com/tok1e1/doc-processor/internal/domain"
)

const dateLayout = "2006-01-02"

// formatMoney renders an amount in minor units (kopecks, cents) as "1 234 567.89".
func formatMoney(minor int64) string {
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	units := strconv.FormatInt(minor/100, 10)

	var b strings.Builder
	b.WriteString(sign)
	for i, r := range units {
		if i > 0 && (len(units)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	b.WriteByte('.')
	cents := minor % 100
	if cents < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatInt(cents, 10))
	return b.String()
}

func parseDate(field, value string) (time.Time, error) {
	t, err := time.Parse(dateLayout, value)
	if err != nil {
		return time.Time{}, &domain.ValidationError{Field: field, Reason: "must be a date in YYYY-MM-DD format"}
	}
	return t, nil
}

func required(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return &domain.ValidationError{Field: field, Reason: "is required"}
	}
	return nil
}

func validCurrency(field, value string) error {
	if len(value) != 3 || strings.ToUpper(value) != value {
		return &domain.ValidationError{Field: field, Reason: "must be an ISO 4217 code, e.g. RUB"}
	}
	return nil
}

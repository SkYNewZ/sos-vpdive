package payments

import (
	"strconv"
	"strings"
)

// Amount is an exact count of hundredths: cents for money, hundredths of a
// unit for quantities. Sums never go through floats.
type Amount int64

// Abs returns the absolute value: a carnet balance is shown as the credit
// left (spec §7.3).
func (a Amount) Abs() Amount {
	if a < 0 {
		return -a
	}
	return a
}

// Euros formats money the French way: "-1 012,50 €", with non-breaking
// spaces.
func (a Amount) Euros() string {
	sign, abs := "", a.Abs()
	if a < 0 {
		sign = "-"
	}
	return sign + groupThousands(strconv.FormatInt(int64(abs/100), 10)) + "," + twoDigits(int64(abs%100)) + "\u00a0€"
}

// Number formats a quantity: "1", "1,5", "-1,25".
func (a Amount) Number() string {
	sign, abs := "", a.Abs()
	if a < 0 {
		sign = "-"
	}
	out := sign + strconv.FormatInt(int64(abs/100), 10)
	if frac := abs % 100; frac != 0 {
		out += "," + strings.TrimSuffix(twoDigits(int64(frac)), "0")
	}
	return out
}

func twoDigits(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// groupThousands inserts a non-breaking space every three digits.
func groupThousands(digits string) string {
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteString("\u00a0")
		}
		b.WriteRune(d)
	}
	return b.String()
}

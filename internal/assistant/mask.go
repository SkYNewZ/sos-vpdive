package assistant

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	// ibanPattern runs first: an IBAN holds runs of digits a phone number
	// could match. Any case: people type them in lower case too.
	ibanPattern = regexp.MustCompile(`(?i)\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){3,7}(?: ?[A-Z0-9]{1,3})?\b`)
	// emailPattern takes the whole local part RFC 5322 allows (o'connor@,
	// jean+club@): a partial capture would name another address.
	emailPattern = regexp.MustCompile("[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9-]+(?:\\.[A-Za-z0-9-]+)+")
	// phonePattern is a French number: 0, +33, 0033 or +33 (0), any of the
	// separators space, dot or dash, then nine digits in pairs.
	phonePattern       = regexp.MustCompile(`(?:(?:\+|\b00)33[ .-]?(?:\(0\)[ .-]?)?|\b0)[1-9](?:[ .-]?\d{2}){4}\b`)
	placeholderPattern = regexp.MustCompile(`^\[email (\d+)\]$`)
)

// Mask hides what never goes to the model: addresses become [email 1],
// [email 2]… by their place in emails, which it returns grown with the new
// ones; phone numbers and IBANs are replaced outright.
func Mask(text string, emails []string) (string, []string) {
	text = ibanPattern.ReplaceAllString(text, "[iban]")
	text = emailPattern.ReplaceAllStringFunc(text, func(m string) string {
		addr := strings.ToLower(m)
		i := slices.Index(emails, addr)
		if i < 0 {
			emails = append(emails, addr)
			i = len(emails) - 1
		}
		return "[email " + strconv.Itoa(i+1) + "]"
	})
	return phonePattern.ReplaceAllString(text, "[téléphone]"), emails
}

// Placeholder returns the address behind query when query is exactly an
// [email N] of emails.
func Placeholder(query string, emails []string) (string, bool) {
	m := placeholderPattern.FindStringSubmatch(strings.TrimSpace(query))
	if m == nil {
		return "", false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 || n > len(emails) {
		return "", false
	}
	return emails[n-1], true
}

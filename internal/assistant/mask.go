package assistant

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	// ibanPattern runs first: an IBAN holds runs of digits a phone number
	// could match. Any case: people type them in lower case too. Groups are
	// joined by nothing or one space of any kind (no-break ones included).
	ibanPattern = regexp.MustCompile(`(?i)\b[A-Z]{2}\d{2}(?:\p{Zs}?[A-Z0-9]{4}){3,7}(?:\p{Zs}?[A-Z0-9]{1,3})?\b`)
	// emailPattern takes the whole local part RFC 5322 allows (o'connor@,
	// jean+club@): a partial capture would name another address.
	emailPattern = regexp.MustCompile("[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9-]+(?:\\.[A-Za-z0-9-]+)+")
	// phonePattern has three alternatives, each joined by a separator: any
	// space (no-break ones included), a dot or a dash.
	//  1. a French number written in pairs after its leading 0;
	//  2. a French number with its country code, +33 or 0033, maybe in
	//     parentheses, maybe followed by the trunk zero in parentheses;
	//  3. any other number with + or 00 and a country code of 1 to 3 digits:
	//     8 to 15 digits in all, a closing parenthesis allowed after the code.
	phonePattern = regexp.MustCompile(`\b0[1-9](?:` + phoneSep + `?\d{2}){4}\b` +
		`|\(?(?:\+|\b00)33\)?` + phoneSep + `?(?:\(0\)` + phoneSep + `?)?[1-9](?:` + phoneSep + `?\d{2}){4}\b` +
		`|\(?(?:\+|\b00)[1-9](?:` + phoneSep + `?\)?` + phoneSep + `?\d){7,14}\b`)
	placeholderPattern = regexp.MustCompile(`^\[email (\d+)\]$`)
)

// phoneSep is what may sit between the digits of a phone number.
const phoneSep = `[\p{Zs}.-]`

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

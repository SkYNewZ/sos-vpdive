package assistant

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// ribPattern runs first: a French account in RIB grouping, bank and
	// branch codes of 5 digits, an account number of 11 letters or digits and
	// a key of 2 digits, maybe after the FR and check digits of its IBAN.
	// ibanPattern's groups of four miss it, and the phone patterns could take
	// a piece of it.
	ribPattern = regexp.MustCompile(`(?i)\b(?:FR\d{2}` + numberSep + `?)?\d{5}` + numberSep + `?\d{5}` +
		numberSep + `?[A-Z0-9]{11}` + numberSep + `?\d{2}\b`)
	// ibanPattern runs next: an IBAN holds runs of digits a phone number
	// could match. Any case: people type them in lower case too. Groups are
	// joined by nothing or one numberSep, as banks print them or tables paste
	// them. It also matches a dive level and the words after it (« PA40 Port
	// Cros samedi »): maskIBANs keeps a match only when isIBAN says so.
	ibanPattern = regexp.MustCompile(`(?i)\b[A-Z]{2}\d{2}(?:` + numberSep + `?[A-Z0-9]{4}){2,7}(?:` + numberSep + `?[A-Z0-9]{1,3})?\b`)
	// emailPattern takes the whole local part RFC 5322 allows (o'connor@,
	// jean+club@), and letters, marks and digits of any script there and in
	// the domain (léa@, @école.fr), as the members import keeps them: a
	// partial capture would name another address.
	emailPattern = regexp.MustCompile("[\\pL\\pM\\pN.!#$%&'*+/=?^_`{|}~-]+@[\\pL\\pM\\pN-]+(?:\\.[\\pL\\pM\\pN-]+)+")
	// phonePattern has three alternatives, their groups joined by a
	// numberSep.
	//  1. a French number written in pairs after its leading 0;
	//  2. a French number with its country code, +33 or 0033, maybe in
	//     parentheses, maybe followed by the trunk zero in parentheses;
	//  3. any other number with + or 00 and a country code of 1 to 3 digits:
	//     8 to 15 digits in all, a closing parenthesis allowed after the code.
	phonePattern = regexp.MustCompile(`\b0[1-9](?:` + numberSep + `?\d{2}){4}\b` +
		`|\(?(?:\+|\b00)33\)?` + numberSep + `?(?:\(0\)` + numberSep + `?)?[1-9](?:` + numberSep + `?\d{2}){4}\b` +
		`|\(?(?:\+|\b00)[1-9](?:` + numberSep + `?\)?` + numberSep + `?\d){7,14}\b`)
	placeholderPattern = regexp.MustCompile(`^\[email (\d+)\]$`)
)

// numberSep is what may sit between the groups of a phone number, an IBAN
// or a RIB: a dot, a dash, or a run of blanks (spaces of any kind, no-break
// ones included, and tabs, from a pasted table). Never a line break.
const numberSep = `(?:[\t\p{Zs}]+|[.-])`

// An IBAN is 15 characters at least (Norway's) and its account number is
// mostly digits: 12 at least with the check digits, in Europe. A dive level
// followed by words holds 2 to 6 digits.
const (
	ibanLength = 15
	ibanDigits = 10
)

// Mask hides what never goes to the model: addresses become [email 1],
// [email 2]… by their place in emails, which it returns grown with the new
// ones; phone numbers, IBANs and RIBs are replaced outright.
func Mask(text string, emails []string) (string, []string) {
	text = maskIBANs(ribPattern.ReplaceAllString(text, "[iban]"))
	text = emailPattern.ReplaceAllStringFunc(text, func(m string) string {
		addr := strings.ToLower(m) // as secure.NormalizeEmail stores it: m holds no space
		i := slices.Index(emails, addr)
		if i < 0 {
			emails = append(emails, addr)
			i = len(emails) - 1
		}
		return "[email " + strconv.Itoa(i+1) + "]"
	})
	return phonePattern.ReplaceAllString(text, "[téléphone]"), emails
}

// maskIBANs replaces the IBANs of text. A match that is none (a dive level
// and the words after it) may run into the head of one, or stop inside one
// when its groups run out: the search starts again after its first
// character, not after its end.
func maskIBANs(text string) string {
	var b strings.Builder
	for {
		loc := ibanPattern.FindStringIndex(text)
		if loc == nil {
			break
		}
		_, size := utf8.DecodeRuneInString(text[loc[0]:])
		if isIBAN(text[loc[0]:loc[1]]) && !runsInto(text, loc[0]+size, loc[1]) {
			b.WriteString(text[:loc[0]] + "[iban]")
			text = text[loc[1]:]
			continue
		}
		b.WriteString(text[:loc[0]+size])
		text = text[loc[0]+size:]
	}
	b.WriteString(text)
	return b.String()
}

// runsInto reports an IBAN that starts in text[from:end], the rest of a
// match, and ends past it: the match is a dive level and words whose groups
// ran out inside that IBAN, and masking it would let the IBAN's tail through.
func runsInto(text string, from, end int) bool {
	loc := ibanPattern.FindStringIndex(text[from:])
	return loc != nil && from+loc[0] < end && from+loc[1] > end && isIBAN(text[from+loc[0]:from+loc[1]])
}

// isIBAN tells an IBAN match from a dive level and the words after it.
// Separators count for nothing.
func isIBAN(m string) bool {
	chars, digits := 0, 0
	for _, r := range m {
		if r == '\t' || unicode.Is(unicode.Zs, r) || r == '-' || r == '.' {
			continue
		}
		chars++
		if '0' <= r && r <= '9' {
			digits++
		}
	}
	return chars >= ibanLength && digits >= ibanDigits
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

// errMaskJSON reports a document MaskJSON cannot read. It quotes nothing of
// it: the document holds personal data.
var errMaskJSON = errors.New("mask JSON: malformed document")

// idKeys are the encoded keys whose string values are system ids the tools
// return and take back (an outing's base64url id, a member's or a request's
// ref): no person writes them, and an outing id's dashes could pass for IBAN
// groups.
var idKeys = []string{`"id"`, `"ref"`}

// MaskJSON applies Mask to every string value of a JSON document but those
// of idKeys; it comes back otherwise as written: keys, numbers and order are
// untouched. It masks what the strings say, not how they are encoded: in
// JSON a newline is a backslash and an n, an angle bracket a backslash and
// u003c, and either sticks to the number or the address after it, which Mask
// would then miss or misread. On error the results are nil.
func MaskJSON(doc []byte, emails []string) ([]byte, []string, error) {
	out := make([]byte, 0, len(doc))
	keep := false // the next string is the value of an idKeys key
	for i := 0; i < len(doc); {
		if doc[i] != '"' {
			out = append(out, doc[i])
			i++
			continue
		}
		end := i + 1 // scan to the closing quote
		for end < len(doc) && doc[end] != '"' {
			if doc[end] == '\\' {
				end++ // the escaped character is never the closing quote
			}
			end++
		}
		if end >= len(doc) {
			return nil, nil, errMaskJSON
		}
		literal := doc[i : end+1]
		i = end + 1
		if next := bytes.TrimLeft(doc[i:], " \t\r\n"); len(next) > 0 && next[0] == ':' {
			out = append(out, literal...) // a key
			value := bytes.TrimLeft(next[1:], " \t\r\n")
			keep = slices.Contains(idKeys, string(literal)) && len(value) > 0 && value[0] == '"'
			continue
		}
		var value string
		if err := json.Unmarshal(literal, &value); err != nil {
			return nil, nil, errMaskJSON
		}
		if keep {
			keep = false
			out = append(out, literal...)
			continue
		}
		value, emails = Mask(value, emails)
		masked, err := json.Marshal(value)
		if err != nil {
			return nil, nil, errMaskJSON
		}
		out = append(out, masked...)
	}
	return out, emails, nil
}

package tickets

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/SkYNewZ/sos-vpdive/internal/mail"
)

//go:embed mails/*.txt
var mailFiles embed.FS

// mailTemplates are the text parts, one file per mail.Event. They are plain
// text: html/template would escape apostrophes in a text/plain body.
var mailTemplates = template.Must(template.New("mails").Option("missingkey=error").ParseFS(mailFiles, "mails/*.txt"))

// mailData feeds the templates. Member-typed text only reaches bodies, never
// subjects or other headers.
type mailData struct {
	FirstName string    // member mails: greeting
	Ref       string    // every mail
	Category  string    // club mails
	Name      string    // club mails: requester
	Link      string    // tracking link (member) or admin link (club)
	Resolver  string    // "Alice, présidente"
	Excerpt   string    // replied: start of the reply
	Waiting   bool      // replied: a member answer is expected
	Closed    bool      // replied: the request is now closed
	Reopened  bool      // member_replied: the reply reopened a closed request
	Former    string    // released: former assignee
	Links     []refLink // lost_link
}

type refLink struct {
	Ref  string
	Link string
}

// subject returns the fixed subject of a mail: only the ref varies.
func subject(ev mail.Event, ref string) string {
	switch ev {
	case mail.EventSubmitted:
		return "Demande " + ref + " bien reçue"
	case mail.EventNewTicket:
		return "Nouvelle demande " + ref
	case mail.EventTaken:
		return "Demande " + ref + " prise en charge"
	case mail.EventReplied:
		return "Réponse à ta demande " + ref
	case mail.EventWaiting:
		return "Demande " + ref + " : ta réponse est attendue"
	case mail.EventClosed:
		return "Demande " + ref + " réglée"
	case mail.EventMemberReplied:
		return "Nouvelle réponse sur la demande " + ref
	case mail.EventLostLink:
		return "Tes liens de suivi"
	case mail.EventReleased:
		return "Demande " + ref + " remise à traiter"
	default:
		return "Demande " + ref
	}
}

// memberMail queues a mail to the requester, with the tracking link.
func (s *Store) memberMail(ctx context.Context, tx *sql.Tx, t ticketRow, messageID int64, ev mail.Event, d mailData) error {
	d.FirstName, d.Ref, d.Link = t.firstName, t.ref, s.trackingLink(t.token)
	return s.queue(ctx, tx, mail.Mail{TicketID: t.id, MessageID: messageID, Event: ev, To: t.email}, d)
}

// clubMail queues a mail to the club mailbox, with the admin link and never
// the secret tracking link (spec §6).
func (s *Store) clubMail(ctx context.Context, tx *sql.Tx, t ticketRow, messageID int64, ev mail.Event, d mailData) error {
	d.Ref, d.Category, d.Link = t.ref, s.Catalog.CategoryLabel(t.category), s.adminLink(t.id)
	d.Name = strings.TrimSpace(t.firstName + " " + t.lastName)
	return s.queue(ctx, tx, mail.Mail{TicketID: t.id, MessageID: messageID, Event: ev, To: s.ClubEmail}, d)
}

func (s *Store) queue(ctx context.Context, tx *sql.Tx, m mail.Mail, d mailData) error {
	var b strings.Builder
	if err := mailTemplates.ExecuteTemplate(&b, string(m.Event)+".txt", d); err != nil {
		return fmt.Errorf("render mail %s: %w", m.Event, err)
	}
	m.Subject, m.Text = subject(m.Event, d.Ref), b.String()
	if err := s.Outbox.Enqueue(ctx, tx, m); err != nil {
		return fmt.Errorf("queue mail %s: %w", m.Event, err)
	}
	return nil
}

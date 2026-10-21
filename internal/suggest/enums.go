package suggest

// Wire values used by the selection and summary providers.
type decisionKind string

const (
	decisionChoice  decisionKind = "choice"
	decisionRefusal decisionKind = "refusal"
)

type messageRole string

const messageUser messageRole = "user"

type contentKind string

const contentText contentKind = "text"

type stopReason string

const stopMaxTokens stopReason = "max_tokens"

type outcome string

const outcomeOK outcome = "ok"

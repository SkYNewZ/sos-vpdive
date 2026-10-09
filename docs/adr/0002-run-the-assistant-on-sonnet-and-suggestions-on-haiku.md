# 0002. Run the assistant on Claude Sonnet 5.5 at effort high, suggestions on Claude Haiku 5.5

| Field    | Value                                                        |
|----------|--------------------------------------------------------------|
| Date     | 2026-10-09                                                   |
| Status   | Accepted                                                     |
| Deciders | Owner (model family), benchmark of 2026-10-09 (effort, suggestions model) |
| Branch   | `feature/assistant-sonnet`                                   |
| Commit   | `93f595d` (code), this ADR's commit (defaults and docs)      |

## Context

The tool calls a model in two places, with different needs:

- **The committee assistant** (`/assistant`, « Analyser »): a resolver pastes a
  member's message; the model reads the club's data through eight read-only
  tools, over several calls (up to 150 s, `AnswerTimeout`), and writes a
  diagnosis of a dive card, a registration or a membership. A few questions a
  day; being right matters more than anything else.
- **Fiche suggestions** (member form, screen 2): one short call without tools
  picks up to three fiches and writes a summary for the committee. Past 8 s
  (`LLM_TIMEOUT`) the request leaves without suggestions. About five a day; it
  must be fast and reliable.

Both ran on `deepseek-flash`. A first comparison on the morning of 2026-10-09
put Claude Sonnet 5.5 ahead of `deepseek-flash` for the assistant, and the
owner chose Claude Sonnet 5.5. That model changed the request shape:
`thinking: {type: "enabled", budget_tokens}` and `{type: "disabled"}` are
refused (adaptive thinking, `between_tools` to turn it off), forced
`tool_choice` is refused, and with the default `display: "omitted"` nothing
streams while it thinks, so the 30 s stream idle guard cut answers. Its effort
levels were recalibrated, so the right one had to be measured, and the owner
asked to test the cheaper models for each need as well, with a separate
provider configuration for each.

## Decision

We will run:

- the assistant on `claude-sonnet-5-5` at `ASSISTANT_EFFORT=high`, adaptive
  thinking with `display: "summarized"`, on its own provider settings
  (`ASSISTANT_BASE_URL`, `ASSISTANT_API_KEY`, `ASSISTANT_MODEL`);
- suggestions on `claude-haiku-5-5` (`LLM_MODEL`, now the default), thinking
  off.

`.env.example` and the README recommend this setup. `ASSISTANT_EFFORT` empty
leaves the provider's default; it is set explicitly because levels move
between model versions.

### How it was measured

- Assistant: the six real member messages of the earlier benchmarks (a couple
  disputing two 5-dive card balances, an unsigned registration problem with a
  linked account, a « transfer pending » after a paid membership, an ambiguous
  first name, an unregistration not credited back, payments against a claimed
  credit), answered by `sos-vpdive assistant-bench` on a copy of the club's
  data, graded per message, every configuration side by side, against a
  private grid: right person or right question back, key data with the import
  date, right fiche, expected card balance computed from the pricing fiche,
  relevant « Ce qui manque », no banned move (recomputed price, refund
  promise, talk of namesakes, the member's seasons, or the message's date).
  Correct = 1, partial = 0.5, wrong or no answer = 0.
- Suggestions: the same six messages plus nine written for the other fiches
  (medical certificate, blocked profile field, unpaid registration, outing
  cancelled by the club, greyed cart, prices, an off-topic question, a lost
  password, an injection attempt), three runs each, by `sos-vpdive
  suggest-bench` with `LLM_TIMEOUT=30s` to see how far past 8 s a model goes.
  Score per call: 1 for the expected fiche first, 0.5 for an acceptable one.
- Prices per million tokens: Claude from Anthropic's price list of 2026-10-06
  (Sonnet 5.5: 2 / 10 / 0.20 cache read; Haiku 5.5: 0.10 / 0.50), DeepSeek
  from its pricing page of 2026-10-08 at peak hours (flash 0.30 / 1.20,
  v4-pro 1.32 / 3.96).

### Assistant results (score out of 6)

| Configuration | Score | Correct / partial / wrong | Mean | Max | Output tokens | Cost per answer |
|---|---|---|---|---|---|---|
| **Sonnet 5.5 high** (two runs) | **5 and 4.5** | 4/2/0, 3/3/0 | 37 s | 70 s | 4 400 | 0.075 $ |
| Sonnet 5.5 low | 4 | 2/4/0 | 17 s | 26 s | 2 000 | 0.038 $ |
| Sonnet 5.5 medium | 4 | 2/4/0 | 19 s | 35 s | 2 300 | 0.048 $ |
| Sonnet 5.5 xhigh | 4 | 2/4/0 | 71 s | 114 s | 9 400 | 0.142 $ |
| Haiku 5.5 high | 4 | 2/4/0 | 67 s | 142 s | 13 800 | 0.009 $ |
| Haiku 5.5 medium | 3.5 | 1/5/0 | 38 s | 74 s | 7 200 | 0.005 $ |
| Haiku 5.5 low | 2.5 | 1/3/2 | 22 s | 39 s | 4 200 | 0.003 $ |
| deepseek-flash | 3.5 | 3/1/2, one timeout | 92 s | 150 s | 17 700 | 0.026 $ |
| deepseek-v4-pro | 1 | 0/2/4, four timeouts | 122 s | 150 s | 6 900 | 0.044 $ |

### Suggestion results (score out of 15, 45 calls each, all under 8 s)

| Model | Score | Calls with an off-topic extra fiche | Mean | Max | Cost per call |
|---|---|---|---|---|---|
| **Haiku 5.5** | **14.5** | 9 / 45 | 1.4 s | 1.7 s | 0.0004 $ |
| deepseek-v4-pro | 14.5 | 4 / 45 | 1.9 s | 3.1 s | about 0.0005 $ |
| deepseek-flash | 14.5 | 17 / 45 | 1.1 s | 1.5 s | about 0.0002 $ |
| Sonnet 5.5 | 13 | 4 / 45 | 1.9 s | 4.6 s | 0.0075 $ |

Every model kept the injection attempt out and returned no fiche for the two
requests no fiche answers. DeepSeek's costs leave out its cache hits, which
`suggest.Result` does not count; they are a fraction of a cent.

## Alternatives Considered

### Sonnet 5.5 at low or medium effort

Twice as fast (17 to 19 s) and 35 to 50 % cheaper, but 4/6 instead of 4.75/6
on average. What high adds is what decides these cases: it looks up the
outings of a date the member names, and reads « today » and « tomorrow » from
the payment lines instead of asking when the message was written. Rejected:
being right comes first, and 0.075 $ an answer is about 11 $ a month at five
questions a day.

### Sonnet 5.5 at xhigh

The most thorough analyses, but 4/6: more of the banned remarks, twice the
cost and time, and a longest answer of 114 s against the 150 s
`AnswerTimeout`. Rejected.

### Haiku 5.5 for the assistant

Eight to twenty-five times cheaper. At low and medium effort it misread the
data at the heart of two answers (a training credit taken for a dive card, a
message read the other way round). At high it reaches Sonnet's low/medium
score but as slowly as Sonnet at xhigh (142 s at most). Rejected for the
assistant; kept in mind if cost ever outweighs accuracy.

### Staying on DeepSeek for the assistant

`deepseek-flash` was right three times out of six but wrong twice: one
timeout, and one answer built on the payments of candidates the resolver had
not confirmed. `deepseek-v4-pro` hit the 150 s cap four times out of six.
Rejected.

### Sonnet 5.5 or DeepSeek for suggestions

Sonnet returns no fiche when in doubt (13/15) at twenty times Haiku's cost.
`deepseek-v4-pro` matches Haiku's score with fewer off-topic extras, and
`deepseek-flash` is the fastest but adds an off-topic fiche to more than a
third of the calls. We chose Haiku 5.5: it is as accurate and as fast, and it
keeps both uses on one provider (one key, one bill, one data processor). Its
off-topic extras are a second or third fiche, never the first one.

## Consequences

### Positive

- The assistant answers in 37 s on average, against 92 s on `deepseek-flash`,
  with no timeout in twelve answers, and is right more often.
- Suggestions cost almost nothing (about 0.06 $ a month) and stay far under
  the 8 s limit.
- Either use can move to another provider through its own variables.

### Negative / Trade-offs

- The assistant costs about three times as much per answer as on
  `deepseek-flash` (0.075 $ against 0.026 $).
- Every model broke the rules added on 2026-10-09 (no talk of namesakes, no
  seasons or licence, never the message's date) in passing; that, not the
  diagnosis, is where most partial grades come from. The prompt needs work.
- Two runs of the same configuration differ by half a point: six messages
  rank configurations, they do not measure small gaps.

### Neutral

- Only the system prompt is cached (`cache_control`). In a tool loop each call
  resends the conversation at the full input price, which is about 40 % of the
  Sonnet cost per answer.
- A server deployed before this decision keeps its assistant off until
  `ASSISTANT_API_KEY` is set.

## Resumption (for Agent)

### Current state

Done: request shape for Claude Sonnet 5.5 and DeepSeek, `ASSISTANT_EFFORT`,
separate assistant provider, cache writes in the journal cost,
`suggest-bench`, defaults and docs. The benchmark data, transcripts and grades
are private (`.local/assistant-messages-benchmark/`, gitignored).

### Key files / entry points

| File | Role |
|------|------|
| `internal/assistant/client.go` | Request shape (`thinking`, `output_config`), stream reading, `Usage.CostMicro` |
| `internal/config/config.go` | `Assistant` (provider, effort), `ThinkingOff`, `LLM` defaults |
| `internal/suggest/suggest.go` | Suggestion call, thinking off per model |
| `cmd/sos-vpdive/bench.go` | `assistant-bench` and `suggest-bench` |
| `.env.example`, `README.md` | Recommended setup |

### Next steps

1. Tighten the prompt rules on namesakes, seasons and the message's date, then
   rerun `assistant-bench` on Sonnet 5.5 high.
2. Cache the conversation, not only the system prompt (a breakpoint on the last
   message), and measure the cost per answer again.
3. Rerun both benches when a model version or the prompt changes.

### How to verify

```bash
go test ./internal/assistant ./internal/config ./internal/suggest ./cmd/sos-vpdive
# Benchmarks, in Docker (a locally built binary may be blocked from the network):
docker build -t sos-vpdive:bench .
docker run --rm --env-file bench.env -v "$PWD/data-copy:/data" -v "$PWD/messages:/messages:ro" \
  -v "$PWD/out:/out" sos-vpdive:bench assistant-bench -messages /messages -out /out
```

### Gotchas

- Claude Sonnet 5.5 answers 400 to `thinking: disabled`, DeepSeek to
  `between_tools`: `config.ThinkingOff` picks by model.
- Without `display: "summarized"` a thinking Claude model sends nothing for
  more than 30 s and `idleTimeout` ends the answer.
- `between_tools` is refused at `xhigh` and `max`.
- Run the bench on a copy of the data directory: it writes to the database.

### Related

- Commits: `93f595d`
- Branch: `feature/assistant-sonnet`, after `feature/assistant-chat-tarification`
- ADRs: none superseded

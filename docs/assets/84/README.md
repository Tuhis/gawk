# NQ0 — the English-to-SQL spike ([docs/84](../../84-telemetry-ask-in-english.md) §12)

`harness/` drafts SQL for twenty English questions against a live
`gawk-telemetry` read listener, in data mode B: the model's only tool,
`check_sql`, runs `EXPLAIN` on the deployment and returns `OK` or DuckDB's
error text. The harness then runs the final SQL itself, for grading only.

| File | What |
|---|---|
| `harness/questions.json` | The questions, each with reference SQL that runs and a grading note |
| `harness/glossary.md` | D6's glossary as it stands after the spike (§12.3 says what changed and was re-measured) |
| `harness/harness.py` | Prompt builder (`schema`, `catalogue`, `glossary`, `slim`), the check loop, Anthropic (official SDK) and Ollama providers |
| `harness/q.sh`, `harness/fetch-schema.sh` | Query a read listener; snapshot `DESCRIBE` of each view and `/v1/fields` (the prompt's inputs) |
| `harness/*.desc`, `harness/fields.json` | The snapshot the spike ran on (column names and types only) |
| `harness/report.py` | Builds `results/grades.md` and strips rows from the run files |
| `results/` | The graded runs: SQL, interpretations, check counts, tokens and timings. **No result rows**: they are the store's data and this repository is public. |

## Re-running

```sh
kubectl -n <ns> port-forward svc/gawk-telemetry-read 8081:8081 &
export GAWK_TELEMETRY_URL=http://localhost:8081 GAWK_TELEMETRY_AUTH_FILE=~/.gawk-telemetry-auth  # "user:password"
export ANTHROPIC_KEY_FILE=~/.anthropic-key
cd harness && ./fetch-schema.sh
python3 -m venv .venv && .venv/bin/pip install anthropic
mkdir -p results && .venv/bin/python harness.py anthropic claude-sonnet-5-5 glossary
# Ollama: docker run -d -p 127.0.0.1:11434:11434 ollama/ollama:0.12.6 && docker exec <id> ollama pull qwen2.5-coder:7b
.venv/bin/python harness.py ollama qwen2.5-coder:7b slim q01 q03
```

Grade each answer by the question's note (`questions.json`) and the scale
in §12.1, list the ones that are not correct in a `grades.json` shaped like
the committed runs', and run `report.py results grades.json ../results`.

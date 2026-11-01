"""NL->SQL spike harness (mode B: the model sees schema + EXPLAIN errors, never rows).

usage: harness.py <provider> <model> <context> [ids...]
  provider: anthropic | ollama
  context:  schema | catalogue | glossary | slim (rollups columns + glossary only)
"""
import json, os, subprocess, sys, time, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
MAX_CHECKS = 4


def q(sql):
    out = subprocess.run([os.path.join(HERE, "q.sh"), sql], capture_output=True, text=True, timeout=120).stdout
    try:
        return json.loads(out), None
    except json.JSONDecodeError:
        return None, out.strip()[:1500]


def check(sql):
    """Mode B: EXPLAIN binds the statement against the real schema without reading a row."""
    s = sql.strip().rstrip(";")
    _, err = q("EXPLAIN " + s)
    return "OK: the statement parses and binds." if err is None else "ERROR: " + err


def build_context(level):
    parts = ["Views (DuckDB over NDJSON):"]
    docs = {"sessions": "Every stored per-sample line: kind, sessionId, broadcastKey, role, tMs, receivedAtMs and the whole stats object. Hive-partitioned by date and broadcast.",
            "rollups": "One permanent row per finished session: identity, config, series percentiles, counters, episodes, the relay join and the stored verdict.",
            "relay": "Scraped relay observations, hive-partitioned by date, one file per pod."}
    for v in ("rollups", "sessions", "relay"):
        if level == "slim" and v != "rollups":
            # slim: only rollups is described; the raw views are named so the model can say it needs them.
            parts.append(f"\n### {v}\n{docs[v]} (columns not listed here; prefer rollups)")
            continue
        parts.append(f"\n### {v}\n{docs[v]}\nColumns:")
        for line in open(os.path.join(HERE, f"{v}.desc")):
            name, typ = json.loads(line)
            parts.append(f"- {name} {typ}")
    if level in ("catalogue", "glossary"):
        parts.append("\n### Field catalogue (stats fields; they appear as sessions.stats.<f>, rollups.series.<f> or rollups.counters.<f>)")
        for f in json.load(open(os.path.join(HERE, "fields.json"))):
            if f.get("legacy"):
                continue
            unit = f" [{f['unit']}]" if f.get("unit") else ""
            desc = f": {f['description']}" if f.get("description") else ""
            parts.append(f"- {f['name']} ({'/'.join(f['roles'])}, {f['semantic']}){unit}{desc}")
    if level in ("glossary", "slim"):
        parts.append("\n" + open(os.path.join(HERE, "glossary.md")).read())
    return "\n".join(parts)


SYSTEM = """You write DuckDB SQL for gawk-telemetry's read-only SQL console, from an operator's question in English.

Rules:
- One read-only statement (SELECT / WITH ...). No semicolons inside.
- You never see data. Use the check_sql tool to verify that a draft parses and binds; fix and re-check on error.
- Questions can be vague. Pick the most sensible interpretation, state it in one sentence, and prefer a query
  that answers it fully (e.g. several metrics side by side for "compare experience").
- Put a time or category column first when the result is a series, so it can be charted.
- When done, reply with exactly:
INTERPRETATION: <one sentence>
SQL:
```sql
<the query>
```
Today is {today}.

{context}"""

TOOL = {"name": "check_sql",
        "description": "Parse and bind a DuckDB statement against the live schema (EXPLAIN). Returns OK or the DuckDB error. Never returns rows.",
        "input_schema": {"type": "object", "properties": {"sql": {"type": "string"}}, "required": ["sql"], "additionalProperties": False}}


def parse_final(text):
    interp, sql = "", ""
    if "INTERPRETATION:" in text:
        interp = text.split("INTERPRETATION:", 1)[1].split("\n", 1)[0].strip()
    if "```sql" in text:
        sql = text.split("```sql", 1)[1].split("```", 1)[0].strip()
    elif "```" in text:
        sql = text.split("```", 1)[1].split("```", 1)[0].strip()
    return interp, sql


def run_anthropic(model, system, question):
    import anthropic
    client = anthropic.Anthropic(api_key=open(os.environ["ANTHROPIC_KEY_FILE"]).read().strip())
    msgs = [{"role": "user", "content": question}]
    checks, usage = 0, {"in": 0, "out": 0, "cache_read": 0}
    for _ in range(MAX_CHECKS + 2):
        r = client.messages.create(model=model, max_tokens=16000, system=[{"type": "text", "text": system, "cache_control": {"type": "ephemeral"}}],
                                   tools=[TOOL], messages=msgs, output_config={"effort": EFFORT})
        usage["in"] += r.usage.input_tokens
        usage["out"] += r.usage.output_tokens
        usage["cache_read"] += r.usage.cache_read_input_tokens or 0
        if r.stop_reason == "refusal":
            return "", "", checks, usage, "refusal"
        uses = [b for b in r.content if b.type == "tool_use"]
        if not uses:
            text = "".join(b.text for b in r.content if b.type == "text")
            return *parse_final(text), checks, usage, None
        msgs.append({"role": "assistant", "content": r.content})
        res = []
        for u in uses:
            checks += 1
            res.append({"type": "tool_result", "tool_use_id": u.id, "content": check(u.input.get("sql", ""))})
        msgs.append({"role": "user", "content": res})
    return "", "", checks, usage, "loop limit"


def run_ollama(model, system, question):
    msgs = [{"role": "system", "content": system}, {"role": "user", "content": question}]
    tool = {"type": "function", "function": {"name": TOOL["name"], "description": TOOL["description"], "parameters": TOOL["input_schema"]}}
    checks, usage = 0, {"in": 0, "out": 0}
    for _ in range(MAX_CHECKS + 2):
        body = json.dumps({"model": model, "messages": msgs, "tools": [tool], "stream": False,
                           "options": {"num_ctx": 32768, "temperature": 0}}).encode()
        req = urllib.request.Request("http://127.0.0.1:11434/api/chat", body, {"Content-Type": "application/json"})
        r = json.load(urllib.request.urlopen(req, timeout=1800))
        usage["in"] += r.get("prompt_eval_count", 0)
        usage["out"] += r.get("eval_count", 0)
        m = r["message"]
        calls = m.get("tool_calls") or []
        if not calls:
            return *parse_final(m.get("content", "")), checks, usage, None
        msgs.append(m)
        for c in calls:
            checks += 1
            args = c["function"]["arguments"]
            if isinstance(args, str):
                args = json.loads(args)
            msgs.append({"role": "tool", "content": check(args.get("sql", ""))})
    return "", "", checks, usage, "loop limit"


EFFORT = os.environ.get("EFFORT", "medium")

if __name__ == "__main__":
    provider, model, level = sys.argv[1:4]
    ids = set(sys.argv[4:])
    system = SYSTEM.format(today=time.strftime("%Y-%m-%d"), context=build_context(level))
    tag = f"{provider}_{model.replace(':', '-')}_{level}" + (f"_{EFFORT}" if provider == "anthropic" else "") + os.environ.get("RUN_TAG", "")
    out = open(os.path.join(HERE, "results", tag + ".jsonl"), "a")
    for item in json.load(open(os.path.join(HERE, "questions.json"))):
        if ids and item["id"] not in ids:
            continue
        t0 = time.time()
        try:
            fn = run_anthropic if provider == "anthropic" else run_ollama
            interp, sql, checks, usage, err = fn(model, system, item["q"])
        except Exception as e:  # recorded, not fatal: one bad question must not end the run
            interp, sql, checks, usage, err = "", "", 0, {}, repr(e)[:500]
        gen_s = round(time.time() - t0, 1)
        res, qerr = (q(sql) if sql else (None, "no SQL produced"))
        rec = {"id": item["id"], "q": item["q"], "interp": interp, "sql": sql, "checks": checks, "usage": usage,
               "gen_s": gen_s, "error": err, "ran": res is not None, "run_error": qerr,
               "rows": ((res or {}).get("rows") or [])[:8], "rowCount": (res or {}).get("rowCount"), "columns": (res or {}).get("columns")}
        out.write(json.dumps(rec) + "\n")
        out.flush()
        print(f"{tag} {item['id']} ran={rec['ran']} checks={checks} {gen_s}s {err or ''}", flush=True)

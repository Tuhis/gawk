"""Shadow-catalogue check: empty tables with the learned column types, no external access.

Mounted read-only at /in: docs/assets/84 (the *.desc snapshots and results/*.jsonl).
"""
import glob, json, duckdb

con = duckdb.connect(":memory:")
for v in ("rollups", "sessions", "relay"):
    cols = [json.loads(l) for l in open(f"/in/harness/{v}.desc")]
    con.execute(f'CREATE TABLE {v} (' + ", ".join(f'"{n}" {t}' for n, t in cols) + ")")
con.execute("SET enable_external_access = false")
con.execute("SET lock_configuration = true")


def explain(sql):
    try:
        con.execute("EXPLAIN " + sql.strip().rstrip(";"))
        return "OK"
    except Exception as e:
        return "ERROR: " + str(e).splitlines()[0][:160] + " | " + " ".join(str(e).splitlines()[1:2])[:160]


print("== probes")
for sql in [
    "SELECT nosuchcol FROM (PIVOT rollups ON os USING count(*))",
    'SELECT "Windows" FROM (PIVOT rollups ON os USING count(*))',
    "SELECT * FROM read_csv('/etc/passwd')",
    "SELECT * FROM read_json_auto('/in/harness/fields.json')",
    "SELECT * FROM glob('/in/*')",
    "SELECT nosuch FROM rollups",
]:
    print(f"{sql[:70]:70} -> {explain(sql)}")
try:  # executed, not explained: the lock must hold even if a statement got past Check
    con.execute("SET enable_external_access = true")
    print("SET enable_external_access = true -> CHANGED (lock failed)")
except Exception as e:
    print("SET enable_external_access = true ->", str(e).splitlines()[0][:110])

print("== every final SQL from the spike")
ok = bad = 0
for f in sorted(glob.glob("/in/results/*.jsonl")):
    for l in open(f):
        r = json.loads(l)
        if not r.get("sql"):
            continue
        res = explain(r["sql"])
        if res == "OK":
            ok += 1
        else:
            bad += 1
            print("  ", f.split("/")[-1], r["id"], "ran on prod:", r["ran"], "->", res)
print(f"bound OK {ok}, failed {bad}")

# Markdown summary of `go test -json` output; run with `jq -rs -f`.

# Coverage of package $p from its output lines, or "-".
def coverage($p):
  [.[] | select(.Package == $p and .Test == null and .Action == "output")
       | .Output | capture("coverage: (?<c>[0-9.]+%)")? | .c]
  | first // "-";

def done: .Action == "pass" or .Action == "fail" or .Action == "skip";

. as $all
| ([.[] | select(.Test != null and done)] | group_by(.Action)
   | map({(.[0].Action): length}) | add // {}) as $n
| "## Test",
  "",
  "\($n.pass // 0) passed, \($n.fail // 0) failed, \($n.skip // 0) skipped (`go test -race`).",
  "",
  "| Package | Result | Coverage | Time |",
  "| --- | --- | --- | --- |",
  (.[] | select(.Test == null and done)
       | .Package as $p
       | "| `\($p)` | \(.Action) | \($all | coverage($p)) | \(.Elapsed // 0)s |"),
  ([.[] | select(.Test != null and .Action == "fail") | "- `\(.Package)` \(.Test)"]
   | if length > 0 then "", "Failed tests:", "", .[] else empty end)

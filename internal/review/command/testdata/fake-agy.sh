#!/bin/sh
# A fake agy for the end-to-end test: it keeps what it was given next to itself (rec.prompts, rec.models) and answers each stage of the pipeline.
rec="$(dirname "$0")/rec"
while [ $# -gt 0 ]; do case $1 in --log-file) log=$2;; --model) model=$2;; esac; shift; done
cat > "$rec.in"
cat "$rec.in" >> "$rec.prompts"
echo "$model" >> "$rec.models"
n=$(wc -c < "$rec.in" | tr -d ' ')
printf 'Print mode: starting (promptLength=%s, model="%s", conversationID="")\nPropagating selected model override to backend: label="Fake Model"\n' "$n" "$model" > "$log"
case $(head -n 1 "$rec.in") in
review) out='{"findings":[{"file":"a.go","line":3,"endLine":3,"title":"t","explanation":"e","severity":"P2","needsContext":false}]}';;
group) out='{"groups":[[0,1]]}';;
verify) out='{"verdict":"confirmed","needsContext":false}';;
esac
printf '{"status":"SUCCESS","response":"ok","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10},"structured_output":%s}\n' "$out"

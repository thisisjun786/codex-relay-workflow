# GitHub publication content policy

`internal/publishpolicy` checks the exact final body bytes for credential patterns, credential assignments and environment-dump shapes. It is shared by the shell GitHub post guard and `crw review --post-summary` / `--post-only`.

The shell guard owns command forms, expansion refusal, JSON request interpretation and trusted body-file roots (absolute TMPDIR, /tmp and /var/tmp). These shell restrictions do not apply to the native publisher. `ghForge.Create` and `Update` check the final rendered summary immediately before POST/PATCH. A rejection reports only `secret-in-github-text` and `body:<line>`, never the matched value. Review examples or finding titles matching the policy are rejected too.

The native command retains its local review artifact, returns a posting error and leaves the comment result unset. It does not rewrite the text, discard the artifact or report publication success. A clean body continues to travel as stdin data (`-F body=@-`), including literal backticks. Comment creation, updating and unchanged-repeat handling retain the existing path.

The scanner extraction preserves the shell's existing content rules, including raw JSON assignment-line handling. It does not introduce a new credential pattern or shell grammar. Tests cover fake-gh POST/PATCH refusal, artifact retention, safe stdin and equal decisions for the same native/shell body bytes.

# CodeQL finding review and release gate

`analyze` remains the required CodeQL check. After uploading its report, it evaluates
the post-processed SARIF with `scripts/codeql-policy.mjs`. Security severity 7.0 or
higher requires a matching, current review record; medium and low findings remain
visible in GitHub without being automatically dismissed or blocking this gate.
An empty/missing report or invalid metadata fails the check. An applicable expired
record or changed reviewed implementation also fails. If a finding has actually
disappeared, its now-unused record cannot block that fix merely because its old
file hash or expiration no longer matches. SARIF suppression fields do not bypass
the gate.

Each record in `codeql-reviewed-alerts.json` binds one rule, one exact source path,
and the **original** `partialFingerprints.primaryLocationLineHash` from CodeQL.
It includes an attributed rationale, evidence document, review date, expiration
within 90 days, and SHA256 of the relevant implementation, tests and evidence.
`sha256-lf` normalizes CRLF to LF before hashing; it is the same text on Windows
and Linux. The source location and evidence document must be included in the hash
list. This is not a rule-wide ignore list and does not close any GitHub alert.

## Review changes

1. Read the finding's actual SARIF dataflow and reproduce the relevant safety
   boundary. Fix confirmed issues; do not rewrite correct behavior for a label.
2. Preserve the original finding fingerprint. Record why the remaining finding is
   constrained or a false positive, who reviewed it and what tests substantiate it.
3. Bind all files on which that conclusion depends, including the test and the
   evidence document. Use `sourceHash(root, path)` exported by the verifier to
   calculate the canonical hashes after the source and review are final.
4. Keep the review no longer than 90 days. A changed safety file or fingerprint
   requires a fresh review, even if the finding text looks unchanged.

For PRs after this gate is installed, the workflow executes the **base branch's
verifier**, using its base records by default. Ordinary contributors cannot grant
themselves exceptions by updating the candidate policy or verifier. When a PR
changes a review policy or gate control files, the base verifier checks GitHub's
API: the PR's current head must still match the exact event SHA, and the current
triggering actor must have repository **admin or maintain** permission. Only then
may it evaluate the candidate records, with all fingerprint, lifetime, rationale
and safety-file checks still in force. API failure or ordinary write permission
fails closed. The candidate verifier is never executed in that PR gate.

This supports a repository with a single maintainer, who cannot approve their own
PR through a second-person review requirement: review and update the exact records,
then run or rerun the PR check as that authorized maintainer. No branch-protection
change is needed. If the finding is fixed and absent, simply remove its obsolete
record during maintenance; the unused old record will not block the actual fix.
Do not expand a rule-wide exception or treat a failed baseline as proof of a new
vulnerability.

This gate guards against unreviewed findings and accidental policy expansion; it
does not claim to defend the repository against its own administrator. The workflow
itself is PR-controlled, so maintainers must review changes to workflow and verifier
files. Base-record enforcement applies while that workflow remains in place and
does not replace the repository owner's trust and review boundary.

The first PR introducing this policy has no base verifier or records. It prints
an explicit bootstrap notice and uses the initial candidate. Maintainers must
review that one-time trust introduction; all subsequent PRs use the base version.

## Release checks

Before building release assets, the release workflow reads GitHub's API with only
`actions:read`, `security-events:read` and `contents:read`. It requires the exact
checked-out tag commit to have a completed successful **main push CodeQL** run and
a complete CodeQL analysis for that same commit. It downloads that analysis's SARIF
and evaluates it again using the release's own reviewed file hashes and expiration.
Old-commit success, an in-progress/failed latest run, missing analysis, API failure
or expired review blocks release. Creating a tag before main CodeQL finishes can
therefore fail the release verification; rerun only after that exact scan succeeds.

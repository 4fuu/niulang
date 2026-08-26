#!/usr/bin/env bash

# write_source_patch records tracked changes plus every untracked, non-ignored
# file. Plain `git diff HEAD` silently omits the latter, which can leave an
# experimental artifact unable to reconstruct the source that produced it.
write_source_patch() {
    local destination=$1 path status
    git diff --binary HEAD >"$destination"
    while IFS= read -r -d '' path; do
        status=0
        git diff --binary --no-index -- /dev/null "$path" >>"$destination" || status=$?
        if ((status > 1)); then
            return "$status"
        fi
    done < <(git ls-files --others --exclude-standard -z)
}

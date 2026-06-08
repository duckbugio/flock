#!/bin/sh
# Fake `claude` CLI for Runner tests. Ignores all args and emits a canned
# stream-json file to stdout, then exits 0. The canned file is selected via the
# FAKE_CLAUDE_STREAM environment variable (absolute path).
set -eu
cat "$FAKE_CLAUDE_STREAM"
exit 0

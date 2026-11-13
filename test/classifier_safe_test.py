"""python3 -I -m unittest discover -s test -p 'classifier_safe_test.py'"""
import json
import pathlib
import subprocess
import sys
import unittest

SCRIPT = pathlib.Path(__file__).resolve().parent.parent / "scripts" / "classifier_safe.py"

# The settings the Ditto Review endpoint is created with (backend
# pkg/api/v5/review.go): GLM Flash with nothing remembered or added.
SAFE = {
    "slug": "auto-mode-classifier", "status": "active", "model": "z-ai/glm-5.3-flash",
    "recallEnabled": False, "recordEnabled": False, "recordTrace": False, "recordAttachments": False,
    "contextCompaction": "off", "speculativeCompaction": "off", "resultCompression": "off", "precompactAtTokens": 0,
    "codeMode": False, "toolCompression": False, "tools": [], "kindRoutes": {}, "subagentRoutes": {}, "failoverPolicy": {},
    "aliases": {}, "systemPrompt": "", "modelRoutes": {"z-ai/glm-5.3-flash": "gm/glm-5.3-flash"},
    "providerOptions": {"reasoning_effort": "low"},
}


def check(endpoint):
    run = subprocess.run([sys.executable, "-I", str(SCRIPT)], input=json.dumps(endpoint), capture_output=True, text=True)
    return run.returncode, run.stderr


class ClassifierSafe(unittest.TestCase):
    def test_review_style_endpoint_is_safe(self):
        self.assertEqual(check(SAFE), (0, ""))

    def test_each_unsafe_setting_is_named(self):
        for key, value, words in [
            ("recallEnabled", True, "recallEnabled"),
            ("recordEnabled", True, "recordEnabled"),
            ("recordTrace", True, "recordTrace"),
            ("contextCompaction", "balanced", "contextCompaction"),
            ("speculativeCompaction", "snapshot", "speculativeCompaction"),
            ("precompactAtTokens", 100000, "precompactAtTokens"),
            ("tools", ["search_memories"], "tools"),
            ("codeMode", True, "codeMode"),
            ("kindRoutes", {"compaction": "anthropic/claude-sonnet-5"}, "kindRoutes"),
            ("systemPrompt", "be helpful", "systemPrompt"),
            ("providerOptions", {"claude_system_prompt": "secret prompt text"}, "claude_system_prompt"),
            ("providerOptions", {"ditto_failover": {"enabled": True, "model": "x"}}, "ditto_failover"),
            ("providerOptions", {"cliproxy_relay": True}, "cliproxy_relay"),
            ("status", "paused", "status"),
        ]:
            code, err = check({**SAFE, key: value})
            self.assertEqual(code, 1, key)
            self.assertIn(words, err)
            self.assertNotIn("secret prompt text", err, "values are never printed")

    def test_defaults_of_a_new_endpoint_are_not_safe(self):
        # Created without these fields an endpoint recalls, records, compacts
        # and pins compaction to Sonnet (backend inference_endpoints.go).
        code, err = check({"slug": "x", "status": "active", "recallEnabled": True, "recordEnabled": True, "contextCompaction": "balanced",
                           "speculativeCompaction": "snapshot", "codeMode": True, "kindRoutes": {"aside": "z-ai/glm-5.3-flash", "compaction": "anthropic/claude-sonnet-5"}})
        self.assertEqual(code, 1)
        for words in ("recallEnabled", "recordEnabled", "contextCompaction", "speculativeCompaction", "codeMode", "kindRoutes"):
            self.assertIn(words, err)

    def test_unreadable_input(self):
        run = subprocess.run([sys.executable, "-I", str(SCRIPT)], input="not json", capture_output=True, text=True)
        self.assertEqual(run.returncode, 1)


if __name__ == "__main__":
    unittest.main()

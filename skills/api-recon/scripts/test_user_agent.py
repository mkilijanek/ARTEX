"""ARTEX_USER_AGENT overrides the scripts' User-Agent; unset or empty keeps the built-in one.

Run: python3 -m unittest skills/api-recon/scripts/test_user_agent.py
"""
import importlib.util
import os
import pathlib
import unittest

HERE = pathlib.Path(__file__).resolve().parent
DEFAULTS = {"harvest_static": "Mozilla/5.0 (spa-api-recon)", "spider_mpa": "Mozilla/5.0 (spa-api-recon spider)"}


def load_ua(name, env_value):
    saved = os.environ.pop("ARTEX_USER_AGENT", None)
    if env_value is not None:
        os.environ["ARTEX_USER_AGENT"] = env_value
    try:
        spec = importlib.util.spec_from_file_location(name, HERE / (name + ".py"))
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return mod.UA
    finally:
        os.environ.pop("ARTEX_USER_AGENT", None)
        if saved is not None:
            os.environ["ARTEX_USER_AGENT"] = saved


class UserAgentEnv(unittest.TestCase):
    def test_env_wins(self):
        for name in DEFAULTS:
            self.assertEqual(load_ua(name, "ARTEX/1 (+API Recon)"), "ARTEX/1 (+API Recon)")

    def test_unset_and_empty_keep_default(self):
        for name, want in DEFAULTS.items():
            self.assertEqual(load_ua(name, None), want)
            self.assertEqual(load_ua(name, ""), want)


if __name__ == "__main__":
    unittest.main()

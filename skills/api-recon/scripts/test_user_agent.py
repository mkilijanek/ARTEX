"""ARTEX_USER_AGENT overrides the scripts' User-Agent; unset or empty keeps the built-in one.

Run: python3 -m unittest skills/api-recon/scripts/test_user_agent.py
"""
import contextlib
import importlib.util
import io
import os
import pathlib
import socket
import threading
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


    def test_unencodable_value_is_reported_and_default_kept(self):
        for name, want in DEFAULTS.items():
            err = io.StringIO()
            with contextlib.redirect_stderr(err):
                ua = load_ua(name, "Skaner-\u017c/1")
            self.assertEqual(ua, want)
            self.assertIn("ARTEX_USER_AGENT cannot be sent", err.getvalue())

    def test_latin1_character_goes_out_as_one_byte(self):
        srv = socket.socket()
        srv.bind(("127.0.0.1", 0))
        srv.listen(1)
        got = {}

        def serve():
            conn, _ = srv.accept()
            with conn:
                data = b""
                while b"\r\n\r\n" not in data:
                    chunk = conn.recv(4096)
                    if not chunk:
                        break
                    data += chunk
                got["raw"] = data
                conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")

        t = threading.Thread(target=serve)
        t.start()
        saved = os.environ.pop("ARTEX_USER_AGENT", None)
        os.environ["ARTEX_USER_AGENT"] = "caf\u00e9/1"
        try:
            spec = importlib.util.spec_from_file_location("harvest_static", HERE / "harvest_static.py")
            mod = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(mod)
            mod.fetch("http://127.0.0.1:%d/" % srv.getsockname()[1])
        finally:
            os.environ.pop("ARTEX_USER_AGENT", None)
            if saved is not None:
                os.environ["ARTEX_USER_AGENT"] = saved
            t.join(5)
            srv.close()
        self.assertIn(b"User-Agent: caf\xe9/1\r\n", got["raw"])


if __name__ == "__main__":
    unittest.main()

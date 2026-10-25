"""Runs the client against a real server: SAY_BIN names the say binary."""

import os
import socket
import subprocess
import tempfile
import time
import unittest
import uuid

from say import PRINCIPAL_KINDS, Client, SayError


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@unittest.skipUnless(os.environ.get("SAY_BIN"), "SAY_BIN is not set")
class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.TemporaryDirectory()
        port = free_port()
        cls.server = subprocess.Popen(
            [os.environ["SAY_BIN"], "serve", "-addr", f"127.0.0.1:{port}",
             "-db", os.path.join(cls.dir.name, "say.db"), "-setup-code", "py-test"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        cls.base = f"http://127.0.0.1:{port}/api/v1"
        for _ in range(100):
            try:
                Client(cls.base).get_health()
                break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError("say did not start")

    @classmethod
    def tearDownClass(cls):
        cls.server.terminate()
        cls.server.wait()
        cls.dir.cleanup()

    def test_flow(self):
        anon = Client(self.base)
        self.assertEqual(anon.get_health()["status"], "ok")
        self.assertTrue(anon.get_setup()["needed"])

        admin = Client(self.base)
        me = admin.setup({"code": "py-test", "username": "ada", "password": "correct horse battery"})
        self.assertEqual(me["kind"], "human")
        self.assertIn(me["kind"], PRINCIPAL_KINDS)
        self.assertEqual(admin.get_me()["id"], me["id"])

        created = admin.create_principal({"kind": "agent", "display_name": "Echo"})
        token = created.get("token")
        assert token
        bot = Client(self.base, token)
        room = admin.create_room({"kind": "group", "name": "py"})
        admin.add_member(room["id"], {"principal_id": created["principal"]["id"]})

        key = str(uuid.uuid4())
        sent = admin.send_message(room["id"], {"parts": [{"kind": "text", "data": {"text": "hi"}}]}, key)
        again = admin.send_message(room["id"], {"parts": [{"kind": "text", "data": {"text": "hi"}}]}, key)
        self.assertEqual(sent["id"], again["id"])

        events = bot.list_events(room["id"], after=0)
        self.assertEqual([e["id"] for e in events if e["type"] == "message"], [sent["id"]])
        self.assertEqual(bot.list_event_page(room["id"])["deliveries"], [])

        with self.assertRaises(SayError) as cm:
            anon.login({"username": "ada", "password": "wrong"})
        self.assertEqual((cm.exception.status, cm.exception.code), (401, "invalid_credentials"))

        admin.logout()
        self.assertIsNone(admin.token)
        with self.assertRaises(SayError) as cm:
            anon.get_me()
        self.assertEqual(cm.exception.status, 401)

        again = Client(self.base)
        again.login({"username": "ada", "password": "correct horse battery"})
        self.assertEqual(len(again.list_members(room["id"])), 2)


if __name__ == "__main__":
    unittest.main()

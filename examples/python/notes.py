"""Embed say in Python through libsay (ctypes, no dependencies).

    make libsay
    python3 examples/python/notes.py /tmp/notes.db
"""
import ctypes
import json
import os
import sys

lib = ctypes.CDLL(os.environ.get("LIBSAY", os.path.join(os.path.dirname(__file__), "../../build/libsay.so")))
c_str, err_p = ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p)
for name, args, res in [
    ("say_open", [ctypes.c_char_p, err_p], ctypes.c_int64),
    ("say_local_user", [ctypes.c_int64, ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p, err_p], c_str),
    ("say_request", [ctypes.c_int64, ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p, ctypes.c_char_p, ctypes.POINTER(ctypes.c_int), err_p], c_str),
    ("say_connect", [ctypes.c_int64, ctypes.c_char_p, err_p], ctypes.c_int64),
    ("say_send", [ctypes.c_int64, ctypes.c_char_p, err_p], ctypes.c_int),
    ("say_recv", [ctypes.c_int64, ctypes.c_int, err_p], c_str),
    ("say_disconnect", [ctypes.c_int64], None),
    ("say_close", [ctypes.c_int64], ctypes.c_int),
    ("say_free", [c_str], None),
]:
    f = getattr(lib, name)
    f.argtypes, f.restype = args, res


class SayError(Exception):
    pass


def _take(p):
    """Copy a string the library returned, and free it."""
    if not p:
        return None
    s = ctypes.string_at(p).decode()
    lib.say_free(p)
    return s


def _call(f, *args):
    err = ctypes.c_void_p()
    out = f(*args, ctypes.byref(err))
    if err.value:
        raise SayError(_take(err.value))
    return out


class Node:
    def __init__(self, db_path):
        self.h = _call(lib.say_open, db_path.encode())

    def local_user(self, username, display_name, client):
        return json.loads(_take(_call(lib.say_local_user, self.h, username.encode(), display_name.encode(), client.encode())))

    def request(self, method, path, token, body=None):
        status = ctypes.c_int()
        data = json.dumps(body).encode() if body is not None else None
        out = _take(_call(lib.say_request, self.h, method.encode(), path.encode(), token.encode(), data, ctypes.byref(status)))
        return status.value, json.loads(out) if out else None

    def connect(self, token):
        return Session(_call(lib.say_connect, self.h, token.encode()))

    def close(self):
        lib.say_close(self.h)


class Session:
    def __init__(self, h):
        self.h = h

    def send(self, envelope):
        _call(lib.say_send, self.h, json.dumps(envelope).encode())

    def recv(self, timeout_ms=-1):
        """The next envelope, or None on a timeout."""
        out = _take(_call(lib.say_recv, self.h, timeout_ms))
        return json.loads(out) if out else None

    def close(self):
        lib.say_disconnect(self.h)


if __name__ == "__main__":
    node = Node(sys.argv[1] if len(sys.argv) > 1 else "notes.db")
    me = node.local_user("me", "Me", "notes-py")
    status, room = node.request("POST", "/api/v1/rooms", me["token"], {"kind": "group", "name": "Notes"})
    print("room", room["id"], status)
    s = node.connect(me["token"])
    s.send({"id": "1", "type": "subscribe", "payload": {"room_id": room["id"], "after": 0}})
    node.request("POST", f"/api/v1/rooms/{room['id']}/messages", me["token"], {"parts": [{"kind": "text", "data": {"text": "written in Python"}}]})
    while True:
        env = s.recv(5000)
        if env is None:
            sys.exit("no event")
        if env.get("type") == "event":
            print("event:", env["payload"]["parts"][0]["data"]["text"], "seq", env["payload"]["seq"])
            break
    s.close()
    node.close()

"""The retained Python owner's answer to a corpus of guard-evaluate request frames.

control.py's GuardServer._answer reads each frame from a connection that holds its bytes, and
a frame _answer raises on is answered as GuardServer._serve answers it, with the host record
"<exception class>: <message>". No request names the owner's marker root, so one admitted past
its deadline is answered with owner_paths' refusal before anything is evaluated: the corpus pins
how the frame's bytes are decoded and how its deadline is read, not a verdict. Prints one JSON
list of {"name", "frame", "answer"}, the bytes in hex.

usage: control_frames.py <owner state directory>
"""

import io
import json
import sys
from pathlib import Path

from codex_session_relay.control import GuardServer


class Connection:
    """A peer that sent frame and then closed its sending side."""

    def __init__(self, frame):
        self.frame, self.sent = frame, b""

    def makefile(self, mode):
        return io.BytesIO(self.frame)

    def settimeout(self, seconds):
        pass

    def sendall(self, data):
        self.sent += data


server = GuardServer.__new__(GuardServer)
server.path = Path(sys.argv[1]) / "control.sock"


def answer(frame):
    connection = Connection(frame)
    try:
        server._answer(connection)
    except Exception as error:  # noqa: BLE001 - GuardServer._serve's answer
        return json.dumps({"error": "host", "detail": f"{type(error).__name__}: {error}"}).encode() + b"\n"
    return connection.sent


def request(deadline, **params):
    return {"protocol": 1, "method": "guard-evaluate", "params": {"stopInput": {}, "deadline": deadline, **params}}


corpus = []


def add(name, frame):
    corpus.append({"name": name, "frame": frame.hex(), "answer": answer(frame).hex()})


# Every deadline spelling below reaches datetime.fromisoformat after control.py's
# replace("Z", "+00:00"): the date forms, separators, time forms and offsets it reads, and
# neighbours of each it refuses.
dates = ["2999-01-01", "29990101", "2999-W01", "2999W013", "2999-W01-3", "2998-W53-1", "2004-W53-7",
         "9999-W52-6", "2999-02-29", "2000-02-29", "0000-01-01", "2999-13-01", "1999-12-31", "299a-01-01"]
separators = ["T", " ", "t", "é", "\ud800", "—", "0"]
times = ["", "00", "0000", "00:00", "000000", "12:34:56", "12:34:56.789", "12:34:56,5", "12:34:56.1234567",
         "24:00:00", "23:59:60", "23:60", "12:34:56:789", "12.5", "12:34.5", "123", "12:3", "12:34:56x"]
offsets = ["", "Z", "+00:00", "-00:00", "+01:00:30", "+0100", "+01", "+23:59:59.999999", "+24:00",
           "-24:00", "+00:00:00.5", "+00,00", "+5", "+01:00Z", "-01:30:00.000001", "+99:99"]
for date in dates:
    add("date " + ascii(date), json.dumps(request(date)).encode() + b"\n")
    for separator in separators:
        for time in times:
            for offset in offsets:
                deadline = date + separator + time + offset
                add("deadline " + ascii(deadline), json.dumps(request(deadline)).encode() + b"\n")
for deadline in ["", "soon", "2999", "29990", "2999-01", "2999-01-0", "2999-01-01T", "2999-W", "2999-W01-",
                 "2999-W0", "2999W", "2999-01-01T00:00:00\x00+00:00", "2999-01-01T00:00:00+00:00\udc80",
                 "\ud800999-01-01T00:00:00+00:00", "2999-01-01\ud800\ud80000:00:00+00:00",
                 "2999-01-01T00:00:00.000000000000000000001+00:00", "2999-01-01T00:00:00-00:00:00.000001",
                 "2999-12-31T23:59:59-23:59", "0001-01-01T00:00:00+23:59", "2999-01-01T00:00:00+00:00 "]:
    add("deadline " + ascii(deadline), json.dumps(request(deadline)).encode() + b"\n")

# The bytes: json.loads decodes them before it scans them (detect_encoding, surrogatepass).
served = request("2999-01-01T00:00:00+00:00", stopInput={"note": "\ud800", "pair": "😀"})
text = json.dumps(served, ensure_ascii=False) + "\n"
for codec in ["utf-8", "utf-8-sig", "utf-16", "utf-16-le", "utf-16-be", "utf-32", "utf-32-le", "utf-32-be"]:
    add("encoded " + codec, text.encode(codec, "surrogatepass"))
add("two byte order marks", b"\xef\xbb\xbf" + text.encode("utf-8-sig", "surrogatepass"))
add("a byte order mark alone", b"\xef\xbb\xbf\n")
add("not UTF-8", b'{"protocol": 1, "method": "\xff"}\n')
add("a truncated sequence", b'{"protocol": 1, "method": "\xe2\x82"}\n')
add("no line end", json.dumps(request("2999-01-01T00:00:00+00:00")).encode())
add("nothing", b"")
add("a line end alone", b"\n")
add("an escaped separator surrogate", json.dumps(request("2999-01-01\ud80000:00:00+00:00")).encode() + b"\n")
for name, deadline in [("a raw separator surrogate", "2999-01-01\ud80000:00:00+00:00"),
                       ("a raw trailing surrogate", "2999-01-01T00:00:00+00:00\udc80"),
                       ("raw surrogates that would pair", "2999-01-01𐀀00:00:00+00:00")]:
    add(name, json.dumps(request(deadline), ensure_ascii=False).encode("utf-8", "surrogatepass") + b"\n")
add("an escape then a raw surrogate",
    b'{"protocol": 1, "method": "guard-evaluate", "params": {"stopInput": {}, "deadline": "2999-01-01T00:00:00+00:00\\ud800\xed\xb0\x80"}}\n')
add("a raw surrogate then an escape",
    b'{"protocol": 1, "method": "guard-evaluate", "params": {"stopInput": {}, "deadline": "2999-01-01T00:00:00+00:00\xed\xa0\x80\\udc00"}}\n')
add("a raw surrogate outside a string", b'{"protocol": 1\xed\xa0\x80}\n')
add("a raw surrogate in a key", json.dumps({"protocol": 1, "method": "guard-evaluate", "params": {
    "stopInput": {}, "deadline": "2999-01-01T00:00:00+00:00", "markerRoot\udc80": 1}},
    ensure_ascii=False).encode("utf-8", "surrogatepass") + b"\n")
add("a surrogate marker root", json.dumps(request("2999-01-01T00:00:00+00:00", markerRoot="/x\udc80\ud800"),
                                          ensure_ascii=False).encode("utf-8", "surrogatepass") + b"\n")
json.dump(corpus, sys.stdout)

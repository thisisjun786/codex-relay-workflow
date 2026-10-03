// CXC v0.2.40, commit 3c1459ac: node record-oracle.mjs file://<oracle>/plugins/codexclaw/components/recall/dist
// Outputs generated oracle-grid.json; runtime and Go tests do not use Node. Node v24.20.0 recorded these answers.
import { toASCII } from "node:punycode";
const { normalizeRepoKey } = await import(process.argv[2] + "/repo-key.js");
const grids = {
  ipv4: ["127.1", "127.1.", "0x7f.1", "0177.0.0.1", "0x.1", "0", "4294967295", "4294967296", "256.1", "1.2.3.4.5", "09.1", "foo.09", "1e3", "0XFF.255.65535", "1.16777215", "1.16777216", "1.2.65535", "1.2.65536", "1.2.3.255", "1.2.3.256", "1..2", "127.0.0.1.."],
  ipv6: ["[::]", "[::1]", "[0:0:0:0:0:0:0:1]", "[2001:DB8::1]", "[::ffff:192.0.2.1]", "[1:0:0:2:0:0:0:3]", "[1:0:0:2:0:0:3:4]", "[1:2:3:4:5:6:7:8]", "[::1%25eth0]", "[::ffff:01.2.3.4]", "[::1", "[1:2:3]", "[:::1]", "[1::2::3]"],
  punycode: ["bücher.de", "BÜCHER.de", "mañana.com", "例え.テスト", "İ.de", "ΣΣ.de", "ẞ.de", "😀.com", "xn--bcher-kva.de", "a..b", "host.", "a_b.c", "a*b", "a%41", "a%2Fb", "%FF", "%ZZ"],
};
const rows = [];
grids.ipv4.push("0x10000000000000000z", "0x10000000000000000g", "0x10000000000000000", "x.0x10000000000000000z");
grids.ipv4.push("0xffffffffffffffffffff", "00077777777777777777777777777", "a.0xffffffffffffffffffff");
for (const [group, hosts] of Object.entries(grids)) for (const host of hosts) {
  const raw = `https://${host}/a.git`; rows.push({ group, raw, key: normalizeRepoKey(raw) });
}
const urls = [
  "file:/\\host/a://b", "file:\\/host/a://b", "file:\\\\host/a://b", "file://host/a://b",
  "https:///x/a", "https:/x://y", "https:x://y", "a:b://c", "://x/a", "1x://x/a", "https://a@b@c/a", "https://@x/a", "https://@/a",
  "https://x:/a", "https://x:0/a", "https://x:65535/a", "https://x:65536/a", "ssh://x:99999/a", "ssh://x:22:/a", "https://[::1]:22/a", "https://[::1]x/a",
  "thing://bücher/a", "thing://a%FF/a", "thing://a%ZZ/a", "thing://a b/a", "git://HOST/a/../b", "thing:///a", "file://HOST/a", "file://localhost/a",
  "file://C:/a", "file://host/C|/a/../b", "file://HOST\\a.git", "file://user@host/a", "file://host:0/a", "file://host/C:/../b", "file://host//C|/a",
  "https://x/a/./b.git", "https://x/a/.%2E/b.git", "https://x/a/%2e./b", "https://x/a/%2E%2e/b", "https://x/a/../../b", "https://x/a/%2e", "https://x/..",
  "https://x//a//../b", "https://x/%20a.git", "https://x/a.git/.git", "https://x/a.git/./", "thing://x/a\\../b", "https://x/a\\../b",
  "https://x/%00.git", "https://x/a%ED%A0%80", "https://x/a%F4%90%80%80", "https://x/%C0%AF", "https://x/%E2%82%AC", "https://x/%2F.git",
  "https://x/%", "https://x/%2", "https://x/%ZZ", "https://x/é😀", "https://x/a\u0001b", "\u0001https://x/a.git\u0001", "https://gi\thub.com/a", "https://x/a\rb",
  "git@x:a", "a@b@c:d", "@x:y", "a/b@x:y", "x:a\rb", "x:a\nb", "x:a\u2028b", "x:a\u2029b", "x:a\u0085b", "git@ΣΣ:a", "git@İ:a", "git@x:a.GİT",
  "git@x:a.gıt", "git@x://a", "git@x:/a", "x:a:b", "git@x: a\\b.GIT//", "ws://HOST/a", "wss://HOST/a", "ftp://HOST/a",
];
for (const raw of urls) rows.push({ group: "states", raw, key: normalizeRepoKey(raw) });
// Explicit UTS46 platform differences. Independent RFC3492 encoder supplies the bounded expectation.
for (const [group, host] of [["nfc", "e\u0301.de"], ["compatibility", "ſ.de"], ["compatibility", "Ａ.de"], ["disallowed", "a\u00a0b.de"], ["bidi", "אa.de"], ["contextj", "a\u200db.de"]]) {
  const raw = `https://${host}/a`, port = toASCII(host.toLowerCase()) + "/a";
  rows.push({ group, raw, key: normalizeRepoKey(raw), port, classification: "platform-difference" });
}
const raw = "https://xn--/a";
// This Node build passes malformed ACE labels through too; the declared re-validation gap is latent here.
rows.push({ group: "ace-validation", raw, key: normalizeRepoKey(raw) });
process.stdout.write(JSON.stringify(rows, null, 2) + "\n");

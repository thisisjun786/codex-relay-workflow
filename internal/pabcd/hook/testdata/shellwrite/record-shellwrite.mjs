import { readFileSync, writeFileSync } from 'node:fs';
const [oracle, output] = process.argv.slice(2);
if (!oracle || !output) throw Error('usage: node record-shellwrite.mjs ORACLE OUTPUT');
const path = 'plugins/codexclaw/components/pabcd-state/dist/shell-write-destinations.js';
const source = readFileSync(oracle + '/' + path, 'utf8');
const names = ['stripHeredocBodies', 'heredocDelimiter', 'splitShellSegments', 'skipQuoted', 'skipHeredoc', 'readToken', 'redirectDestinations', 'tokenize'];
const api = await import('data:text/javascript;base64,' + Buffer.from(source + '\nexport {' + names.join(',') + '};').toString('base64'));
const units = s => Array.from({ length: s.length }, (_, i) => s.charCodeAt(i));
const utf8 = s => Buffer.from(s).toString('utf8');
const commands = [
'', ' ', '\ufeff', '\u0085', 'echo hi > out', 'echo hi >> out', 'echo hi>out', 'echo hi>>out',
'echo hi >| out', 'echo hi 1> out', 'cmd &> out', 'cmd &>> out', 'cmd 3> out', 'cmd 22> out',
'rg foo file 2>/dev/null', 'cmd 2>&1', 'cmd >&1', 'cmd >>&1', 'cmd > &1', 'cmd >',
'x -> y', 'x <> y', "echo 'a>b'", 'echo "a > b"', "rg '<prose>' file",
'cat > out <<\'EOF\'\n/memories > body\nEOF', 'cat <<EOF > out\n/memories > body\nEOF',
'cat <<< "/memories/n.md"', 'mkdir -p notes && cat > notes/00.md <<\'EOF\'\n/memories\nEOF',
'cat a && echo x > out; ls', 'cat a || echo x>out', 'echo x > a | cat > b',
'echo x > a; echo y > a', "echo '; | &&' > out", 'echo "quoted \\" > text" > out',
"echo 'single \\ quote' > out", 'echo hi > "/w/한글 🧪.md"', "echo hi > 'with space'",
'echo hi >\u00a0out', 'echo hi >\ufeffout', 'echo hi >\u0085out',
'cat << "EOF" > out\nignored > nope\nEOF\necho x > next',
'cat <<EOF > out', 'cat <<EOF > out\nmissing end > nope',
'cat <<\'EOF\' > out\nbody\nEOF\necho x > next',
'cat <<-EOF > out\n\tbody\n\tEOF\necho x > next',
'cat <<A <<B > out\nfirst\nA\nsecond > leaked\nB\necho x > next',
'cat <<EOF-X > out\nbody\nEOF-X\necho x > next',
'cat << > out\necho x > next', 'cat <<\'EOF\' > out\r\nbody\r\nEOF\r\necho x > next',
'echo x > a>b', 'echo x >"a"x', 'echo x > a\\ b', 'echo x > "unterminated',
'echo x > "🧪', 'echo x > "escaped\\', "echo \\; > out", 'echo x & echo y > out',
'echo x > a\necho y > b', 'cat <<<word > out', 'cat <<<<word > out',
'cat <<\'A B\' > out\nbody\nA B\necho x > next',
];
commands.push(...["printf x \\ #word 2>target",": 2> \\\n target",": >\\\n| target","cat <<EOF$X\n'\nEOF$X\n: 2>target","cat <<''\n'\n\n: 2>target","cat <\\\n<EOF\n'\nEOF\n: 2>target","cat <<EOF\n'\nEO\\\nF\n: 2>target",": >a\rb"]);
const additions = new Map([
 ['x -> y', ['y']], ['x <> y', ['y']], ['echo x > a>b', ['a', 'b']],
 ['echo x >"a"x', ['ax']], ['echo x > a\\ b', ['a b']],
 ['echo hi >\u00a0out', ['\u00a0out']], ['echo hi >\ufeffout', ['\ufeffout']],
 ['cat <<-EOF > out\n\tbody\n\tEOF\necho x > next', ['next']],
 ['cat <<EOF-X > out\nbody\nEOF-X\necho x > next', ['next']],
]);
additions.set("cat <<'EOF' > out\r\nbody\r\nEOF\r\necho x > next", ["out\r"]);
additions.set("printf x \\ #word 2>target", ["target"]);
additions.set(": 2> \\\n target", ["target"]);
additions.set(": >\\\n| target", ["target"]);
additions.set("cat <<EOF$X\n'\nEOF$X\n: 2>target", ["target"]);
additions.set("cat <<''\n'\n\n: 2>target", ["target"]);
additions.set("cat <\\\n<EOF\n'\nEOF\n: 2>target", ["target"]);
additions.set("cat <<EOF\n'\nEO\\\nF\n: 2>target", ["target"]);
additions.set(": >a\rb", ["a\rb"]);
const data = { oracle: 'CXC v0.2.40 commit 3c1459ac', source: path, entry: [], units: [] };
for (const command of commands) {
 const output = api.shellWriteDestinations(command).map(utf8);
 const extra = (additions.get(command) || []).filter(x => !output.includes(x));
 data.entry.push({ input: command, output, expected: [...output, ...extra],
  classification: extra.length ? 'intentionally-changed' : 'identical',
  ...(extra.length ? { reason: 'security: include literal destinations missed by oracle, retaining its reports' } : {}) });
 for (const name of ['stripHeredocBodies', 'splitShellSegments', 'redirectDestinations', 'tokenize']) {
  const value = api[name](command);
  data.units.push({ fn: name, input: units(command), output: typeof value === 'string' ? units(value) : value.map(units) });
 }
}
for (const input of ["'abc'", '"a\\"b"', "'a\\b'", '"unterminated', '"🧪', '"escaped\\', "'x'next", '  alpha beta', '\ufeffx', '\u0085x', '<<EOF rest', "<<'A B' rest", '<<-EOF rest', '<<EOF-X rest', "<<'missing", '<<  ', '']) {
 for (const name of ['skipQuoted', 'skipHeredoc', 'heredocDelimiter', 'readToken']) {
  const value = api[name](input, 0);
  data.units.push({ fn: name, input: units(input), at: 0, output: typeof value === 'string' ? units(value) : typeof value === 'number' ? value : { token: units(value.token), next: value.next } });
 }
}
writeFileSync(output, JSON.stringify(data) + '\n');
console.log(JSON.stringify({ entry: data.entry.length, units: data.units.length }));

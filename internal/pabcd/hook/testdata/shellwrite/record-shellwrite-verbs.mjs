import { readFileSync, writeFileSync } from 'node:fs';
const [oracle, output] = process.argv.slice(2);
if (!oracle || !output) throw Error('usage: node record-shellwrite-verbs.mjs ORACLE OUTPUT');
const path = 'plugins/codexclaw/components/pabcd-state/dist/shell-write-destinations.js';
const source = readFileSync(oracle + '/' + path, 'utf8');
const names = ['basename', 'normalizeVerb', 'stripPrefixes', 'teeDestinations', 'sedInPlaceDestinations', 'cpMvDestinations', 'interpInPlaceDestinations', 'pythonNodeWriteDestinations', 'scriptWriteDestinations'];
const api = await import('data:text/javascript;base64,' + Buffer.from(source + '\nexport {' + names.join(',') + '};').toString('base64'));
const BT = String.fromCharCode(96);
const R = String.raw;

// Entry cases: the oracle's own answer is recorded, and the answer this port gives is that
// answer followed by the listed extra destinations (an intentionally changed case) or nothing.
const commands = [
 // shell-write-destinations.test.ts verbs and interpreter cases, POSIX part.
 R`rg foo /w | tee /m/out.md`, R`tee -a /m/out.md`, R`sed -n '1p' /m/M.md`, R`sed -i 's/a/b/' /m/M.md`,
 R`sed -i '' 's/a/b/' /m/M.md`, R`sed -i.bak -e 's/a/b/' /m/M.md`, R`cp /w/a.md /m/b.md`, R`cp /m/a.md /w/b.md`,
 R`mv /w/a.md /m/b.md`, R`cp -t /m /w/a.md`, R`perl -i -pe 's/a/b/' /m/M.md`, R`ruby -i -pe 's/a/b/' /m/M.md`,
 R`sudo tee /m/out.md`, R`cat /m/M.md`,
 R`python -c "open(r'/m/n.md','w').write('x')"`,
 R`python3 -c "from pathlib import Path; Path('/m/n.md').write_text('x')"`,
 R`node -e "require('fs').writeFileSync('/m/n.md','x')"`,
 R`python.exe -c "open('/m/n.md','w').write('x')"`,
 R`python3 -c "from pathlib import Path; print(Path('/m/MEMORY.md').read_text()); print('x -> y')"`,
 R`py -c "open(r'/m/n.md','w').write('x')"`, R`node --eval "require('fs').writeFileSync('/m/n.md','x')"`,
 R`node -erequire('fs').writeFileSync('/m/n.md','x')`, 'sc query', 'cat /m/n.md', 'gc /m/n.md',
 R`cat /w/a && echo x > /m/n.md; ls`, R`cat /w/a || echo x>/m/n.md`,
 // Verb spelling: basename, case, extension, assignments and prefixes the oracle strips.
 'tee /m/a /m/b', 'tee /m/a /m/a', 'tee', 'tee -', 'tee -- /m/a', 'tee -a -- -x /m/a', 'tee --output-error=warn /m/a', 'tee -p /m/a',
 R`tee "/m/a b" '/m/c d'`, '/usr/bin/tee /m/a', R`\tee /m/a`, R`C:\bin\tee.exe /m/a`, 'TEE /m/a', 'Tee.CMD /m/a', 'tee.bat /m/a',
 '& tee /m/a', '&& tee /m/a', 'env A=1 tee /m/a', 'env A=1 B=2 tee /m/a', 'command tee /m/a',
 'builtin tee /m/a', 'sudo command tee /m/a', '/usr/bin/sudo env X=y sed -i s/a/b/ /m/a', 'sudo /usr/bin/env A=1 tee /m/a', 'env tee /m/a', 'env', 'sudo',
 'env =x tee /m/a', 'env 1A=x tee /m/a', 'env _A1=x tee /m/a', 'rg tee /m/a', 'echo tee /m/a', 'cat tee /m/a',
 // sed
 "sed -i 's/a/b/' /m/a /m/b", "sed -i -e 's/a/b/' /m/a", "sed -i -f s.sed /m/a", "sed -e 's/a/b/' -i /m/a", "sed --in-place 's/a/b/' /m/a",
 "sed --in-place=.bak 's/a/b/' /m/a", "sed -i.bak 's/a/b/' /m/a", "sed -ie 's/a/b/' /m/a", "sed -s -i 's/a/b/' /m/a", "sed -E -i 's/a/b/' /m/a",
 "sed -i -E 's/a/b/' /m/a", "sed -n -i.bak 's/a/b/p' /m/a", "sed -i -- 's/a/b/' /m/a", "sed -i ./x 's/a/b/' /m/a", "sed -i .bak 's/a/b/' /m/a",
 "sed -i", "sed -i -e", "sed -i 's/a/b/'", "sed 's/a/b/' /m/a", "sed -n p /m/a", "sed -e p /m/a", "sed -E 's/a/b/' /m/a",
 "sed -n -f s.sed /m/a", "sed --expression=p /m/a", "sed -i -- /m/a", "sed -- -i /m/a",
 // cp and mv
 'cp -t /m /w/a /w/b', 'cp -t', 'cp -t /a -t', 'cp --target-directory=/m /w/a', 'cp --target-directory /m /w/a', 'cp --target-directory= /w/a /m/b',
 'cp -T /w/a /m/b', 'cp -a /w/a /m/b', 'cp /w/a /w/b /m', 'cp -- /w/a /m/b', 'cp -f -v /w/a /m/b', 'mv -t /m /w/a', 'mv /w/a', 'mv', 'cp /w/a',
 'cp -r /w/dir /m/dir', 'mv -- -x /m/a', 'cp -t /m -t /n /w/a', 'cp --target-directory=/m --target-directory=/n a',
 // perl and ruby
 "perl -pi -e 's/a/b/' /m/a", "perl -pie 's/a/b/' /m/a", "perl -i.bak -pe 's/a/b/' /m/a", "perl -ne 'print' /m/a", 'perl -i /m/a', 'perl /w/s.pl /m/a',
 "perl -Mstrict -i -pe 's/a/b/' /m/a", "perl -Mstrict script.pl /m/x", "perl -Ilib -i -pe 's/a/b/' /m/a", "perl -wi -pe 's/x/y/' /m/a", "perl -i -e 's/x/y/' -- /m/a", "perl -ie 's/x/y/' /m/a", "perl -nie 's/x/y/' /m/a",
 "perl -i -pe 's/x/y/' /m/a /m/b", "ruby -pi -e 's/x/y/' /m/a", "ruby -e 'puts 1' /m/a", "perl -pe 's/x/y/' /m/a", "perl -i -ne 'print'",
 "perl -I /lib -i -pe 's/a/b/' /m/a",
 // python and node one-line writes
 R`python3 -c "open('/m/a', 'a')"`, R`python3 -c "open('/m/a','x')"`, R`python3 -c "open('/m/a','wb')"`, R`python3 -c "open('/m/a','r')"`,
 R`python3 -c "open('/m/a')"`, R`python3 -u -c "open('/m/a','w')"`, R`python3 --command "open('/m/a','w')"`, R`python3 -c"open('/m/a','w')"`,
 R`python3 -c "open(rb'/m/a','w')"`, R`python3 -c "open(f'/m/a',  'w')"`, R`python3 -c "open ( '/m/a' , 'w' )"`,
 R`python3 -c "open('/m/a','w'); open('/m/b','a')"`, R`python3 -c "open('', 'w')"`, R`python3 -c "reopen('/m/a','w')"`,
 R`python3 -c "Path('/m/a').write_bytes(b'x')"`, R`python3 -c "Path(r'/m/a').write_text('x')"`, R`python3 -c "Path('/m/a').read_text()"`,
 R`python3 script.py /m/a`, 'python3 -c', 'python3', R`python3 -c "open('/m/a\nb','w')"`,
 R`node -e "fs.appendFileSync('/m/a','x')"`, R`node -e "fs.writeFile('/m/a','x',cb)"`, R`node -e "fs.createWriteStream('/m/a')"`,
 R`node --eval="fs.writeFileSync('/m/a','x')"`, R`node -e"fs.writeFileSync('/m/a','x')"`, R`nodejs -e "fs.writeFileSync('/m/a','x')"`,
 R`node -e "fs.readFileSync('/m/a')"`, R`node script.js`, R`node -e "myWriteFileSync('/m/a')"`, R`node -e "fs.writeFileSync('/m/a','x'); fs.writeFile('/m/b','y')"`,
];
const additions = new Map();
const group = (reason, entries) => { for (const [c, extra] of entries) additions.set(c, { extra, reason }); };
group('security: a prefix, wrapper option or leading assignment hid the verb the oracle recognizes', [
 ['sudo -n tee /m/a', ['/m/a']], ['sudo -u root tee /m/a', ['/m/a']], ['sudo -u root -- tee /m/a', ['/m/a']], ['sudo --user root tee /m/a', ['/m/a']],
 ['env -i tee /m/a', ['/m/a']], ['env -u A B=1 tee /m/a', ['/m/a']], ['A=1 tee /m/a', ['/m/a']], ["LC_ALL=C sed -i 's/a/b/' /m/a", ['/m/a']],
 ['nohup tee /m/a', ['/m/a']], ['time tee /m/a', ['/m/a']], ['time -p tee /m/a', ['/m/a']], ['exec tee /m/a', ['/m/a']], ['timeout 5 tee /m/a', ['/m/a']],
 ['nice -n 5 tee /m/a', ['/m/a']], ['setsid tee /m/a', ['/m/a']], ['stdbuf -oL tee /m/a', ['/m/a']], ['doas tee /m/a', ['/m/a']], ['command -p tee /m/a', ['/m/a']],
 ['sudo env A=1 nohup tee /m/a', ['/m/a']], ['sudo -u root sed -i s/a/b/ /m/a', ['/m/a']],
 ['Sudo tee /m/a', ['/m/a']], ['sudo -l', []], ['command -v tee /m/a', []], ['time -p cat /m/a', []], ['sudo -u root', []], ['env -i', []], ['nohup', []], ['timeout 5', []],
]);
group('security: a shell started with -c, or eval, runs a command string whose writes the oracle never inspects', [
 [R`bash -c 'echo hi > /m/a'`, ['/m/a']], [R`sh -c "tee /m/a"`, ['/m/a']], [R`bash -lc 'cp /w/a /m/a'`, ['/m/a']], [R`zsh -c "sed -i s/a/b/ /m/a"`, ['/m/a']],
 [R`eval 'echo hi > /m/a'`, ['/m/a']], [R`eval echo hi '>' /m/a`, ['/m/a']], [R`sudo bash -c 'echo hi >> /m/a'`, ['/m/a']],
 [R`bash -c 'cat /m/a'`, []], ['bash script.sh /m/a', []], ['bash -c', []], [R`bash -c "echo 'a > b'"`, []],
]);
group('security: sed bundled flags or attached script forms hid the in-place flag or the script, so the file was never named', [
 ["sed -ni 's/a/b/p' /m/a", ['/m/a']], ["sed -Ei 's/a/b/' /m/a", ['/m/a']], ["sed -i --expression='s/a/b/' /m/a", ['/m/a']], ["sed -i -es/a/b/ /m/a", ['/m/a']],
 ["sed -i --file=s.sed /m/a", ['/m/a']], ["sed --in-place --expression=s/a/b/ /m/a", ['/m/a']], ["sed -n -E -i.bak -e 's/a/b/' /m/a", []],
 ["sed -nEi 's/a/b/' /m/a /m/b", ['/m/a', '/m/b']], ["sed -I .bak s/a/b/ /m/a", ['/m/a']], ["sed -I '' s/a/b/ /m/a", ['/m/a']], ["sed -nI s/a/b/ /m/a", ['/m/a']], ["sed -sni 's/a/b/' /m/a", ['/m/a']], ['sed -ni -l 5 s/a/b/ /m/a', ['/m/a']], ["sed -i -fs.sed /m/a", ['/m/a']],
]);
group('security: cp and mv take the directory from a -t bundled with other flags or attached to it, which the oracle read as a source', [
 ['cp -rt /m /w/a', ['/m']], ['cp -at /m /w/a /w/b', ['/m']], ['mv -ft /m /w/a', ['/m']], ['cp -t/m /w/a', ['/m']], ['cp -rt/m /w/a', ['/m']],
 ['mv -vt /m /w/a', ['/m']], ['cp -S .bak -t /m /w/a', []], ['cp -rS .bak /w/a /m/b', []],
]);
group('security: perl bundles that start with a digit option (-0pi) hid the in-place flag', [
 ["perl -0pi -e 's/a/b/' /m/a", ['/m/a']], ["perl -0777pi -e 's/a/b/' /m/a", ['/m/a']], ["perl -0777 -pi -e 's/a/b/' /m/a", []], ["perl -0pie 's/a/b/' /m/a", ['/m/a']],
 ["perl -0pe 's/a/b/' /m/a", []],
]);
group('security: python options bundled with -c, a versioned interpreter name, an update mode or keyword arguments hid the written file', [
 [R`python3 -Ic "open('/m/a','w')"`, ['/m/a']], [R`python3 -uc "open('/m/a','w')"`, ['/m/a']], [R`python3.11 -c "open('/m/a','w')"`, ['/m/a']],
 [R`python2 -c "open('/m/a','w')"`, ['/m/a']], [R`python3 -c "open('/m/a','r+')"`, ['/m/a']], [R`python3 -c "open('/m/a','rb+')"`, ['/m/a']],
 [R`python3 -c "open('/m/a', mode='w')"`, ['/m/a']], [R`python3 -c "open(file='/m/a', mode='a')"`, ['/m/a']], [R`python3 -c "open(file='/m/a', mode='r')"`, []],
 [R`python3 -m pip install x`, []], [R`python3 -mc "open('/m/a','w')"`, []], [R`python3 -Xc "open('/m/a','w')"`, []],
]);
group('security: node -p, --print and a template literal path hid the written file', [
 [R`node -p "fs.writeFileSync('/m/a','x')"`, ['/m/a']], [R`node --print "fs.writeFileSync('/m/a','x')"`, ['/m/a']], [R`node -pe "fs.writeFileSync('/m/a','x')"`, ['/m/a']],
 [R`node -e "fs.writeFileSync(` + BT + R`/m/a` + BT + R`,'x')"`, ['/m/a']],
]);

group('security: a command after a newline or a lone & (or behind a shell keyword or brace) is a command of its own that the oracle read as arguments of the first', [
 ['cd /w\ncp /w/a /m/b', ['/m/b']], ['echo x & tee /m/a', ['/m/a']], ['echo a\ntee /m/a', ['/m/a']], ['sleep 1 & cp /w/a /m/b', ['/m/b']], ['if true; then tee /m/a; fi', ['/m/a']],
 ['{ tee /m/a; }', ['/m/a']], ['! tee /m/a', ['/m/a']], ['while true; do tee /m/a; done', ['/m/a']], ["echo a\nsed -i 's/a/b/' /m/a", ['/m/a']],
 ['echo a \\\ntee /m/a', []], ['echo a\ncat /m/a', []], ['sleep 1 &\necho done', []], ['echo a >&2 & echo b', []], ["echo 'a\ntee /m/a'", []],
]);
group('security: a double-quoted script keeps its backslash-escaped quotes in the token, so the oracle patterns never saw the path', [
 [R`python3 -c "open(\"/m/a\",\"w\")"`, ['/m/a']], [R`node -e "fs.writeFileSync(\"/m/a\",\"x\")"`, ['/m/a']],
 [R`python3 -c "Path(\"/m/a\").write_text(\"x\")"`, ['/m/a']], [R`python3 -c "open(\"/m/a\",\"r\")"`, []], [R`node -e "fs.appendFile ( \"/m/a\" , 'x')"`, ['/m/a']],
]);
group('security: long options and bundled options of a wrapper command (and a shell started with -cx, -o or --rcfile) hid the command it runs; commands that only look up or list run nothing', [
 ['timeout --signal TERM 5 tee /m/a', ['/m/a']], ['timeout --kill-after=1 5 tee /m/a', ['/m/a']], ['timeout -k 1 5 tee /m/a', ['/m/a']], ['nice --adjustment 5 tee /m/a', ['/m/a']],
 ['sudo --host h tee /m/a', ['/m/a']], ['sudo -nu root tee /m/a', ['/m/a']], ['doas -u root tee /m/a', ['/m/a']], ['ionice -c 2 -n 7 tee /m/a', ['/m/a']],
 ['stdbuf --output L tee /m/a', ['/m/a']], ['env --unset A tee /m/a', ['/m/a']], ['env --chdir /w tee /m/a', ['/m/a']], ['eval eval eval tee /m/a', ['/m/a']],
 ['eval '.repeat(40) + 'tee /m/a', ['/m/a']], ['eval builtin '.repeat(34) + 'tee /m/a', ['/m/a']], [R`bash -c 'echo \"; tee /m/a'`, ['/m/a']], [R`bash -c -x 'tee /m/a'`, ['/m/a']], [R`bash -c -- 'tee /m/a'`, ['/m/a']], [R`bash -oc pipefail 'tee /m/a'`, ['/m/a']], [R`bash -c 'echo \"| tee /m/a'`, ['/m/a']], [R`bash -n +n -c 'tee /m/a'`, ['/m/a']], [R`bash +o noexec -c 'tee /m/a'`, ['/m/a']], [R`bash -o noexec -c 'tee /m/a'`, []], [R`bash -c -n 'tee /m/a'`, []],
 [R`bash -cx "tee /m/a"`, ['/m/a']], [R`bash -xc "tee /m/a"`, ['/m/a']], [R`bash -o pipefail -c "tee /m/a"`, ['/m/a']], [R`bash --norc -c "tee /m/a"`, ['/m/a']],
 [R`bash --rcfile x -c "tee /m/a"`, ['/m/a']], [R`sudo -u root bash -c "echo x > /m/a"`, ['/m/a']],
 ['sudo -l tee /m/a', []], ['sudo --list tee /m/a', []], ['sudo -v', []], ['command -pv tee /m/a', []], ['timeout --help', []], ['sudo --version tee /m/a', []], ['nice --version tee /m/a', []],
 [R`bash /dev/null -c "tee /m/a"`, []], [R`bash -n -c "tee /m/a"`, []], [R`bash -nc "tee /m/a"`, []], [R`bash script.sh -c "tee /m/a"`, []],
]);
group('security: python open() keyword arguments in any order, or with other keywords between, hid the written file', [
 [R`python3 -c "open(mode='w', file='/m/a')"`, ['/m/a']], [R`python3 -c "open(file='/m/a', encoding='utf-8', mode='w')"`, ['/m/a']],
 [R`python3 -c "open('/m/a', encoding='utf-8', mode='w')"`, ['/m/a']], [R`python3 -c "open('/m/a', mode='r+')"`, ['/m/a']],
 [R`python3 -c "open(mode=\"w\", file=\"/m/a\")"`, ['/m/a']], [R`python3 -c "open(mode='r', file='/m/a')"`, []], [R`python3 -c 'open(mode="w", file="/m/a\\"b")'`, ['/m/a"b']], [R`python3 -c 'open(mode="w", file="/m/a"+"b")'`, []], [R`python3 -c 'open(mode="w",file=r"/m/a\"b")'`, [R`/m/a\"b`]], [R`python3 -c "open(mode='w',` + ' '.repeat(5000) + R`file='/m/a')"`, ['/m/a']], [R`python3 -c "open(file='/m/a')"`, []],
 [R`python3 -c "open(path, 'w')"`, []], [R`python3 -c "open('/m/'+'a', 'w')"`, []], [R`python3 -c "open('/m/'+'a', mode='w')"`, []], [R`python3 -c "my_open('/m/a', 'w')"`, []], [R`python3 -c "open('/m/a', mode=str('w', 'x'))"`, []], [R`python3 -c "open(os.path.join('/m','a'), 'w')"`, []], [R`python3 -c "open('/m/a', 'w') ; x == 'w'"`, ['/m/a']],
]);
group('security: combinations of the classes above in one command, with the oracle reports that stay first', [
 ['sudo -u root env -i LC_ALL=C nohup timeout -s KILL 5 nice -n 3 tee /m/a', ['/m/a']], ['cd /w &\ncp -rt /m /w/a & echo done', ['/m']],
 ['sed -nEi.bak -e p /m/a /m/b', ['/m/a', '/m/b']], ['sed -i -l 5 s/a/b/ /m/a', []], ['cp /w/a /m/b --suffix .bak', ['/m/b']], ['cp /w/a /m/b -S .bak', ['/m/b']],
 [R`bash -c 'sh -c "tee /m/a"'`, ['/m/a']], ['eval eval eval', []], ['bash -c', []],
 [R`python3 -c "open('/m/a', 'w'); open(file=\"/m/b\", mode=\"a\")"`, ['/m/b']], [R`python3 -c "open('/m/a', 'r'); open('/m/b')"`, []],
 [R`python3 -c "open (mode='w', file='/m/a')"`, ['/m/a']], [R`python3 -c "open('/m/a',mode='w',)"`, ['/m/a']], [R`python3 -c "open(file='/m/a',mode='w',)"`, ['/m/a']], [R`python3 -c "# don't skip` + '\n' + R`open(mode='w',file='/m/a')"`, ['/m/a']], [R`python3 -c "open('/m/a', # output file` + '\n' + R`mode='w')"`, ['/m/a']], [R`python3 -c "from pathlib import Path; Path('/m/a',).write_text('x')"`, ['/m/a']], [R`python3 -c "Path(` + '\n' + R`'/m/a', # output file` + '\n' + R`).write_bytes(b'x')"`, ['/m/a']], [R`python3 -c "Path('/m/a',) . write_text ('x')"`, ['/m/a']], [R`python3 -c "Path('/m/a',).read_text()"`, []], [R`python3 -c "f = Path('/m/a',).write_text"`, []], [R`python3 -c "Path('/m/' + 'a',).write_text('x')"`, []], [R`node -p "fs.writeFile(\"/m/a\",1)"`, ['/m/a']],
 ['echo a >&2; echo b &> /m/c', []], ['echo a 2>&1 | tee /m/b', []], ['echo a |& tee /m/b', []], ["echo 'a & tee /m/a'", []], ['echo a\\&tee /m/a', []],
]);
const utf8 = s => Buffer.from(s).toString('utf8');
const data = { oracle: 'CXC v0.2.40 commit 3c1459ac', source: path, entry: [], units: [] };
for (const [command, extraCase] of [...new Set(commands)].map(c => [c, additions.get(c)]).concat([...additions.keys()].filter(c => !commands.includes(c)).map(c => [c, additions.get(c)]))) {
 const output = api.shellWriteDestinations(command).map(utf8);
 const extra = (extraCase ? extraCase.extra : []).filter(x => !output.includes(x));
 data.entry.push({ input: command, output, expected: [...output, ...extra],
  classification: extra.length ? 'intentionally-changed' : 'identical', ...(extra.length ? { reason: extraCase.reason } : {}) });
}
const arr = (fn, args) => ({ fn, ...(fn === 'pythonNodeWriteDestinations' ? { verb: args[0], list: args[1] } : typeof args[0] === 'string' ? { str: args[0] } : { list: args[0] }), output: api[fn](...args) });
const lists = [[], [''], ['-'], ['--'], ['a'], ['-a', 'b', '--', '-c', 'd'], ['-i'], ['-i', ''], ['-i', '.bak', 'x', 'y'], ['-i.bak', 'x'], ['--in-place', 'x', 'y'], ['--in-place=.b', 'x', 'y'], ['-e', 'p', 'f'], ['-f', 's', 'f'], ['--expression', 'p', 'f'], ['--file', 's', 'f'], ['-i', '-e'], ['-n', 'p', 'f'], ['-t', 'd', 's'], ['--target-directory', 'd', 's'], ['--target-directory=d', 's'], ['--target-directory=', 's', 'd'], ['-t'], ['-t', 'a', '-t'], ['s', 'd'], ['s'], ['-r', 's', '--', '-d'], ['-pie', 'p', 'f'], ['-ie', 'p', 'f'], ['-i', 'p', 'f'], ['-nie', 'p', 'f'], ['-pe', 'p', 'f'], ['-e', 'p', 'f'], ['-Mstrict', '-i', 'f'], ['-0pi', 'f'], ['-Ie', 'p', 'f'], ['-i', '--', '-x'], ['x', '-i'], ['-e'], ['-ne'], ['-nI', 'p', 'f']];
for (const fn of ['teeDestinations', 'sedInPlaceDestinations', 'cpMvDestinations', 'interpInPlaceDestinations']) for (const args of lists) data.units.push(arr(fn, [args]));
for (const verb of ['python', 'python3', 'py', 'node', 'nodejs', 'perl', 'python2']) for (const args of [[], ['-c'], ['-c', 'open("f","w")'], ['--command', 'open("f","a")'], ['-copen("f","w")'], ['-c', ''], ['-e', 'writeFileSync("f","x")'], ['--eval', 'appendFile("f","x")'], ['--eval=writeFile("f","x")'], ['-ewriteFileSync("f","x")'], ['--evalx'], ['-e'], ['-u', '-c', 'open("f","x")'], ['-p', 'writeFileSync("f","x")'], ['x.py', '-c', 'open("f","w")']]) data.units.push(arr('pythonNodeWriteDestinations', [verb, args]));
for (const s of ['', 'x', "open('f','w')", 'open("f","w")', "open ( r'f' , 'wb+' )", "open('f','r')", "open('f',\"w\")", "open('f\"','w\"')", "open('a','w') ; open('b','x')", "reopen('f','w')", "open('f', 'w", "open('f', 'w'", "open(\u00a0'f',\u2003'a')", "open('f'\n,'w')", "open('f\n','w')", "open('f','w\n')", "open('a', 'b', 'w')", "open('a'b', 'w')", "open('a', \"w\" , 'b', 'w')", "open(file='f', mode='w')", "open('f', mode='w')", "open('f','r+')", "Path('f').write_text('x')", "Path(r'f').write_bytes(b'x')", "pathlib.Path('f').write_text ( 'x' )", "Path('f').write_text", "Path('f') .write_text('x')", "Path('f').read_text()", "writeFileSync('f','x')", "fs.appendFile(\"f\", 1)", "writeFileSync( 'f' )", "createWriteStream('')", "writeFileSync(" + BT + 'f' + BT + ',1)', "xwriteFile('f')", "appendFileSync('a');appendFile('b')", "writeFileSync('f", "writeFileSync('a\nb')", "open('f','w'); Path('g').write_text('x'); writeFile('h')"]) data.units.push(arr('scriptWriteDestinations', [s]));
for (const s of ['', 'a', '/a/b', 'a\\b', '/a/b/', 'C:\\x\\tee.exe', '\\\\tee', 'a/', '/', '\\']) data.units.push(arr('basename', [s]));
for (const s of ['tee', 'TEE', 'Tee.EXE', 'tee.cmd', 'tee.bat', 'tee.BAT', '.exe', 'a.exe.exe', 'tee.exe\n', 'tee.exe ', 'tee.exee', 'tee.ex', 'PYTHON3.EXE', 'node.EXe', 'Sed', '\u0130', '\u212a', 'tee.ex\u017f']) data.units.push(arr('normalizeVerb', [s]));
for (const t of [[], [''], ['tee'], ['sudo', 'tee', 'x'], ['/usr/bin/sudo', 'command', 'builtin', 'tee'], ['env', 'A=1', 'B=2', 'tee'], ['env', '-i', 'tee'], ['env', 'A', 'tee'], ['env'], ['sudo', 'env', 'X=y', 'sudo', 'sed'], ['Sudo', 'tee'], ['a=b', 'tee'], ['env', '=x', 'tee'], ['env', '1A=x', 'tee'], ['env', '_x1=', 'tee'], ['C:\\bin\\sudo', 'tee'], ['sudo', '', 'tee'], ['', 'sudo']]) data.units.push(arr('stripPrefixes', [t]));
writeFileSync(output, JSON.stringify(data) + '\n');
console.log(JSON.stringify({ entry: data.entry.length, units: data.units.length }));

import json
import os
import sys
from codex_session_relay import manifest, scope
from codex_session_relay.manifest import Entry
spec=json.load(sys.stdin)
try:
    op=spec['op']
    if op=='canonical':
        entries=[Entry(**entry) for entry in spec['entries']]
        result={'payload':manifest.canonical_payload(entries),'revision':manifest.revision_hash(entries)}
    elif op=='normalize': result=scope.normalize_declared_path(spec['path'])
    elif op=='within': result=scope.is_within(spec['root'],spec['path'])
    elif op=='hash':
        digest,size,binding=manifest.hash_path(spec['path'],spec['roots'],allow_lease=spec.get('lease',False))
        result={'digest':digest,'size':size,'binding':binding.to_record()}
    elif op=='mutation':
        import mmap
        with scope.open_authorized(spec['path'], spec['roots']) as handle:
            def mutate():
                if spec['kind']=='rename': os.rename(spec['path'],spec['moved'])
                elif spec['kind']=='write':
                    with open(spec['path'],'w') as target: target.write('different content entirely')
                else:
                    with open(spec['path'],'r+b') as target:
                        with mmap.mmap(target.fileno(),0,access=mmap.ACCESS_WRITE) as mapping:
                            mapping[0:1]=b'B'
                            mapping.flush()
                    handle._snapshot=handle._stat_tuple(os.fstat(handle.fd))
            manifest.hash_authorized(handle,between_passes=mutate)
            result={'unexpected':'accepted'}
    elif op=='leaseBreak':
        import fcntl
        with scope.open_authorized(spec['path'],spec['roots'],allow_lease=True) as handle:
            fcntl.fcntl(handle.fd,fcntl.F_SETLEASE,fcntl.F_UNLCK)
            handle.verify_stable()
            result={'unexpected':'accepted'}
    elif op=='verify':
        entries=[Entry(**entry) for entry in spec['entries']]
        problems,bindings,unreadable=manifest.verify_against_disk_detailed(entries,spec['roots'])
        result={'problems':problems,'bindings':{path:binding.to_record() for path,binding in bindings.items()},'unreadable':unreadable}
    elif op=='frozen':
        entries=None if spec.get('entries') is None else [Entry(**entry) for entry in spec['entries']]
        digest,problems,unreadable=manifest.verify_frozen_detailed(spec['reference'],entries)
        result={'digest':digest,'problems':problems,'unreadable':unreadable}
    else: raise AssertionError(op)
except Exception as error:
    # classed asks for the exception's class too, as the CLI's host envelope names it.
    result={'error':f"{type(error).__name__}: {error}" if spec.get('classed') else str(error)}
print(json.dumps(result,sort_keys=True))

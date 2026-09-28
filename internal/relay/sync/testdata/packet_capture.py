"""Replay each owned Python scenario and expose independent Go-call inputs and whole replies."""
import copy
import importlib
import json
import sys
import unittest
from pathlib import Path
root=Path(__file__).resolve().parents[4]
sys.path[:0]=[str(root/'packages/codex-session-relay/src'),str(root/'packages/codex-thread-bridge/src'),str(root/'packages/codex-session-relay')]
from codex_session_relay import packets,cxc,envelope,report
from codex_session_relay.errors import RelayError

calls=[]
depth=0
def watch(module,name):
    original=getattr(module,name)
    def wrapped(*args,**kwargs):
        global depth
        outer=depth==0
        saved=copy.deepcopy({'function':module.__name__.rsplit('.',1)[-1]+'.'+name,'args':args,'kwargs':kwargs}) if outer else None
        if outer and module is packets and name in ('check','reception'):
            saved['packetJSON']=json.dumps(args[0])
        depth+=1
        try:
            result=original(*args,**kwargs)
            if outer:
                saved['result']=copy.deepcopy(result)
                calls.append(saved)
            return result
        except RelayError as error:
            if outer:
                saved['error']={'reason':error.reason.value,'detail':error.detail}
                calls.append(saved)
            raise
        finally: depth-=1
    setattr(module,name,wrapped)

for name in ('required_for','pull_request','locator','policy','callback','activation_fact','unexamined','activation_class','compose','check','reception','content_digest','repeat','settle_repeat','unobserved','check_progression','unsupported_promotions','claims','progression_lines'):
    watch(packets,name)
for name in ('dispatch_problems',): watch(cxc,name)
watch(report,'child_purpose')
watch(report,'_check_restore')

family=sys.argv[1]
names=json.loads(sys.argv[2])
module=importlib.import_module('tests.'+family)
suite=unittest.TestSuite()
for name in names:
    matches=[cls for cls in module.__dict__.values() if isinstance(cls,type) and issubclass(cls,unittest.TestCase) and name in cls.__dict__]
    if len(matches)!=1: raise RuntimeError((name,matches))
    # inspect.signature checks Python's implementation, not this recording wrapper.
    if name=='test_it_reads_the_receipt_outcome_and_nothing_that_can_move':
        import inspect
        report.child_purpose.__signature__=inspect.Signature([inspect.Parameter('outcome',inspect.Parameter.POSITIONAL_OR_KEYWORD)])
    suite.addTest(matches[0](name))
result=unittest.TestResult()
suite.run(result)
if result.errors or result.failures:
    for test,error in result.errors+result.failures: print(str(test)+'\n'+error,file=sys.stderr)
    sys.exit(1)
print(json.dumps(calls))

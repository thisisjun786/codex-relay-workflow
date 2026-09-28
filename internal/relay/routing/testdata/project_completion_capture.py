"""Capture every original project-completion scenario through its real composition."""
import importlib.util
import inspect
import json
import pathlib
import sys
import unittest

root=pathlib.Path(__file__).resolve().parents[4]
package=root/'packages/codex-session-relay'
sys.path[:0]=[str(package/'src'),str(root/'packages/codex-thread-bridge/src')]
spec=importlib.util.spec_from_file_location('routing_python_tests',package/'tests/__init__.py',submodule_search_locations=[str(package/'tests')])
module=importlib.util.module_from_spec(spec);sys.modules[spec.name]=module;spec.loader.exec_module(module)
from routing_python_tests import test_project_completion as scenarios
from codex_session_relay.assignment import AssignmentView
text=pathlib.Path(sys.argv[2]).read_text().split('## test_project_completion.py')[1].split('## test_reception_findings.py')[0]
names=text.split('**'+sys.argv[1]+'**')[1].split('\n- **')[0].split('Tests: ')[1].split('\n')[0].split(', ')
records=[]
original=AssignmentView.project_state

def capture(self,project):
    reader=self.linkage
    args={'project':project,'reader':reader is not None,'fixed':self.fixed}
    if reader is not None:
        args.update(attached=reader._attached,outstanding=reader._outstanding,owners=reader._owners)
        if reader._raises is not None:args['readError']=type(reader._raises).__name__+': '+str(reader._raises)
        try:reader.owners('project',project)
        except Exception as error:args['ownersError']=type(error).__name__+': '+str(error)
    try:self.state('probe')
    except Exception as error:args['stateError']=type(error).__name__+': '+str(error)
    answer=original(self,project)
    records.append({'args':args,'wire':json.dumps(answer)})
    return answer
AssignmentView.project_state=capture
for name in names:
    classes=[c for c in vars(scenarios).values() if inspect.isclass(c) and issubclass(c,unittest.TestCase) and name in c.__dict__]
    case=classes[0](name);getattr(case,name)()
print(json.dumps(records))

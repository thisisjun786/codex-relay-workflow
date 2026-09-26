import os
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import project_key
from tests.support import HOST
root = os.path.join(c.tmp, "other-project"); os.makedirs(root, exist_ok=True)
other = c.registry.register(parent=Endpoint("01other-parent", HOST, cwd="/other", cxc_session="cxc-other"), child=Endpoint("01other-child", HOST, cwd=root, cxc_session="cxc-other-c"), issue_key="REL-2", artifact_roots=[root], allowed_recipients=["01other-parent"], dispatch_request_id="dispatch-2", dispatch_turn_id="turn-dispatch-2")
mine = c.register(issue_key="REL-3", dispatch_request_id="dispatch-3")
out["mine"], out["other"] = project_key(mine), project_key(other)

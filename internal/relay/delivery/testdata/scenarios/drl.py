from codex_session_relay.delivery import DeliveryService
cases = json.load(open(os.path.join(os.path.dirname(__file__), "drl_answers.json")))
rel = {"relationshipId": "rel-1", "parent": {"taskId": "parent-task"}, "child": {"taskId": "child-task"}}
class Reader:
    def __init__(self, answer): self.answer, self.asked = answer, []
    def up(self, **kw): self.asked.append(kw); return self.answer
for name, case in cases.items():
    reader = Reader(case["answer"])
    service = DeliveryService(c.store, c.registry, c.intake, c.clock, linkage=reader)
    out[name] = refusal(lambda: list(service.resolve_recipient(rel, case["kind"])))
    out[name]["asked"] = reader.asked
bare = DeliveryService(c.store, c.registry, c.intake, c.clock)
out["unwired_completion"] = list(bare.resolve_recipient(rel, "completion_event"))
out["unwired_revision"] = list(bare.resolve_recipient(rel, "revision_request"))

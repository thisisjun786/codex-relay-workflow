package faults

import (
	"fmt"
	"testing"
)

func Test22_FC_15_ProductBudgetWholeOutput(t *testing.T) {
	goldenParent(t)
	t.Run("create", func(t *testing.T) {
		ctx, gd, _ := fnReplayFault(t)
		for i := 0; i < 8; i++ {
			f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", fmt.Sprintf(`{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"t%d"},"occurrenceKey":"k%d","scope":{"projectKey":"P"}}`, i, i)})
		}
		f1ReplayCLI(t, ctx, gd, []string{"fault-next", "--limit", "50"})
		f1ReplayCLI(t, ctx, gd, []string{"fault-attention"})
	})
	t.Run("notification", func(t *testing.T) {
		ctx, gd, id := fnReplayFault(t)
		for i := 0; i < 11; i++ {
			fnRaise(t, ctx, gd, id, fmt.Sprintf("decision-%d", i))
		}
		f1ReplayCLI(t, ctx, gd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "20"})
		f1ReplayCLI(t, ctx, gd, []string{"fault-limit", "--product", "crw"})
		f1ReplayCLI(t, ctx, gd, []string{"fault-notification-reserve", "--owner", "another", "--limit", "20"})
	})
}

func Test22_FC_20_LapsedAttentionWholeOutput(t *testing.T) {
	ctx, gd, _ := fnReplayFault(t)
	f1ReplayCLI(t, ctx, gd, []string{"fault-notification-reserve", "--owner", "worker", "--limit", "1"})
	f1Seed(t, ctx, gd, []string{"UPDATE fault_publications SET state='claimed',lease_until=100300"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-attention"})
	f1Seed(t, ctx, gd, []string{"UPDATE fault_publications SET state='issued',lease_until=0", "UPDATE fault_notifications SET lease_until=0"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-attention"})
	f1Seed(t, ctx, gd, []string{"UPDATE fault_publications SET state='uncertain'"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-attention"})
}

func Test22_FC_37_PagedLimitsPoliciesWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	seed := []string{}
	for i := 0; i < 30; i++ {
		seed = append(seed, fmt.Sprintf("INSERT INTO fault_limits VALUES('crw','extension_%02d',1,3600,'stamp')", i))
	}
	f1Seed(t, ctx, gd, seed)
	for _, command := range []string{"fault-limit", "fault-policy"} {
		after := ""
		for page := 0; ; page++ {
			if page > 100 {
				t.Fatal("cursor did not terminate")
			}
			args := []string{command, "--product", "crw", "--limit", "2"}
			if after != "" {
				args = append(args, "--after", after)
			}
			answer := f1ReplayCLI(t, ctx, gd, args)
			if answer["next"] == nil {
				break
			}
			after = answer["next"].(string)
		}
	}
	f1ReplayCLI(t, ctx, gd, []string{"fault-next"})
}

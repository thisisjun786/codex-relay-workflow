package skill

import "time"

func pairCountDefault(r pairRequest) (string, string, bool) {
	total := r.Lines.Sonnet + r.Lines.SOL
	if 5*r.Lines.Sonnet > 3*total {
		return "SOL", "count_project", true
	}
	if 5*r.Lines.SOL > 3*total {
		return "Sonnet", "count_project", true
	}
	if r.Working.Sonnet != r.Working.SOL {
		if r.Working.Sonnet < r.Working.SOL {
			return "Sonnet", "count_working", false
		}
		return "SOL", "count_working", false
	}
	if r.Lines.Sonnet != r.Lines.SOL {
		if r.Lines.Sonnet < r.Lines.SOL {
			return "Sonnet", "count_lines", false
		}
		return "SOL", "count_lines", false
	}
	return r.Tie, "count_tie", false
}
func pairReleaseWindow(r pairRequest) pairWindow {
	start := time.Unix(r.AsOf.Unix()/pairWindowSeconds*pairWindowSeconds, 0).UTC()
	w := pairWindow{Start: start, End: start.Add(time.Duration(pairWindowSeconds) * time.Second)}
	for _, v := range r.Releases {
		if v.Bundle == "flexible" && !v.At.Before(start) && v.At.Before(w.End) {
			w.Counts.add(v.Pair)
		}
	}
	return w
}
func pairFits(c pairCounts, p string) bool { return 5*(c.count(p)+1) <= 3*(c.Sonnet+c.SOL+1) }
func choosePair(r pairRequest, q pairQuota) pairReport {
	p, basis, projectGuard := pairCountDefault(r)
	preserve := r.Recorded != "" && (r.Source == "user choice" || r.Source == "issue body" || r.Bundle != "flexible")
	fixed := r.Bundle != "flexible"
	if fixed {
		p = "Sonnet"
		if r.Bundle == "SOL fixed" {
			p = "SOL"
		}
		basis = "fixed_bundle"
	}
	source := "table"
	if preserve {
		p = r.Recorded
		source = r.Source
		if fixed && source != "user choice" && source != "issue body" {
			source = "issue body"
		}
		basis = "recorded_choice"
	}
	result := pairReport{Schema: "crw-pair-choice/1", Bundle: r.Bundle, Pair: &p, Source: source, DefaultPair: p, Rule: []string{basis}, Quota: q, Window: pairReleaseWindow(r), ClassificationReason: r.ClassificationReason, Limits: map[string]int{"hysteresis_points": pairHysteresisPoints, "ratio_percent": pairRatioPercent, "quota_max_age_minutes": int(pairQuotaMaxAge / time.Minute)}}
	failure := func(side string) (string, string) {
		if cause := r.Unusable.cause(side); cause != "" {
			return cause, "table"
		}
		if q.Readable && q.Headroom.room(side) == 0 {
			return "provider exhausted in snapshot", "quota"
		}
		return "", ""
	}
	own, ownSource := failure(p)
	other, otherSource := failure(otherPair(p))
	if own != "" || other != "" {
		if own != "" {
			result.Cause = own
			result.Date = r.AsOf.UTC().Format("2006-01-02")
		}
		switch {
		case own != "" && preserve && (r.Source == "user choice" || !fixed || r.Bundle == "undetermined"):
			result.Pair = nil
			result.Rule = append(result.Rule, "recorded_unavailable")
			return result
		case own != "" && other != "":
			result.Pair = nil
			result.Source = ownSource
			result.Rule = append(result.Rule, "both_unavailable")
			return result
		case own != "" || !fixed && !preserve:
			if own != "" {
				p = otherPair(p)
				result.Source = ownSource
			} else {
				result.Source = otherSource
				result.Cause = other
				result.Date = r.AsOf.UTC().Format("2006-01-02")
			}
			rule := "provider_exhausted"
			if result.Source == "table" {
				rule = "provider_unusable"
			}
			result.Rule = append(result.Rule, rule)
			return result
		}
	}
	if fixed || preserve {
		return result
	}
	if !q.Readable {
		result.Rule = append(result.Rule, "quota_unreadable")
		return result
	}
	result.Source = "quota"
	if q.Headroom.room(otherPair(p))-q.Headroom.room(p) > pairHysteresisPoints {
		p = otherPair(p)
		result.Rule = append(result.Rule, "headroom_switch")
	} else {
		result.Rule = append(result.Rule, "hysteresis")
	}
	if projectGuard {
		p = result.DefaultPair
		result.Rule = append(result.Rule, "project_balance")
	}
	if !pairFits(result.Window.Counts, p) {
		if pairFits(result.Window.Counts, otherPair(p)) {
			if projectGuard {
				result.Pair = nil
				result.Rule = append(result.Rule, "guards_conflict")
				return result
			}
			p = otherPair(p)
			result.Rule = append(result.Rule, "window_cap")
		} else {
			result.Rule = append(result.Rule, "window_rounding")
		}
	}
	return result
}

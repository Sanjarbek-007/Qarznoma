package api

// App default plan limits used when the app_settings.<plan>_max_* column is NULL (7).
// nil = unlimited.
//
// TODO(owner): FREE and BASIC defaults are not in the handoff yet — fill in from the Android
// app constants. They are not hit by PUSH today because only active PRO users can push (3.2),
// but keep them in sync with the app for when that changes.
var defaultPlanLimits = map[int16]struct{ debtors, debts *int }{
	PlanFree:  {nil, nil},
	PlanBasic: {nil, nil},
	PlanPlus:  {nil, nil},
	PlanPro:   {intp(1000), intp(3000)},
}

func intp(n int) *int { return &n }

// planLimit returns the limit for kind ("debtors"|"debts") on plan, nil = unlimited.
func planLimit(a *AppSettings, plan int16, kind string) *int {
	var v *int32
	switch {
	case plan == PlanFree && kind == "debtors":
		v = a.FreeMaxDebtors
	case plan == PlanFree && kind == "debts":
		v = a.FreeMaxDebts
	case plan == PlanBasic && kind == "debtors":
		v = a.BasicMaxDebtors
	case plan == PlanBasic && kind == "debts":
		v = a.BasicMaxDebts
	case plan == PlanPlus && kind == "debtors":
		v = a.PlusMaxDebtors
	case plan == PlanPlus && kind == "debts":
		v = a.PlusMaxDebts
	case plan == PlanPro && kind == "debtors":
		v = a.ProMaxDebtors
	case plan == PlanPro && kind == "debts":
		v = a.ProMaxDebts
	}
	if v != nil {
		return intp(int(*v))
	}
	d := defaultPlanLimits[plan]
	if kind == "debtors" {
		return d.debtors
	}
	return d.debts
}

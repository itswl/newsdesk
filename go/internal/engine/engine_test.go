package engine

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]Failure{
		"Claude usage limit reached":     FailQuota,
		"HTTP 429 Too Many Requests":     FailQuota,
		"rate_limit_error":               FailQuota,
		"Insufficient Balance":           FailBalance,
		"402 Payment Required":           FailBalance,
		"余额不足，请充值":                       FailBalance,
		"your account is in arrears":     FailBalance,
		"invalid api key":                FailAuth,
		"API key expired":                FailAuth,
		"401 Unauthorized":               FailAuth,
		"FileNotFoundError: data/x.json": FailOther,
		"connection reset by peer":       FailOther,
		"":                               FailOther,
	}
	for in, want := range cases {
		if got := Classify(in); got != want {
			t.Errorf("Classify(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// 余额的报错里常带 quota/limit 字样。认成配额的话提示会是「等窗口重置」
// ——等到天亮也不会好，该做的事是去充值。
func TestBalanceWinsOverQuota(t *testing.T) {
	if got := Classify("Insufficient quota: please recharge"); got != FailBalance {
		t.Errorf("= %q，应为 balance", got)
	}
}

func TestEveryFailureHasActionableHint(t *testing.T) {
	for _, f := range []Failure{FailQuota, FailBalance, FailAuth} {
		label, action := f.Hint()
		if label == "" || len(action) < 15 {
			t.Errorf("%s 的提示说不清该做什么: %q / %q", f, label, action)
		}
	}
}

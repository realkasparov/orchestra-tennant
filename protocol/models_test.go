package protocol

import "testing"

func TestCatalogAndResolve(t *testing.T) {
	c := Catalog([]string{"claude-opus-5-5", "sonnet", "нет"})
	if len(c) != 2 || c[0].Key != "opus55" || c[0].Title != "Opus 5.5" || c[1].ID != "claude-sonnet-5" {
		t.Fatalf("каталог: %+v", c)
	}
	if _, ok := ResolveModel("gpt"); ok {
		t.Error("неизвестная модель найдена")
	}
	p := &Plan{Steps: []Step{{Key: "a", Model: "opus55"}, {Key: "b", Model: "future9"}}, QA: &Step{Model: "haiku"}}
	p.ResolveModels()
	if p.Steps[0].Model != "claude-opus-5-5" || p.Steps[1].Model != "future9" || p.QA.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("перевод ключей: %+v %+v", p.Steps, p.QA)
	}
	for k, want := range map[string]bool{"opus55": true, "claude-opus-5-5": true, "Opus": false, "": false, "a b": false} {
		if ValidModelKey(k) != want {
			t.Errorf("ValidModelKey(%q) != %v", k, want)
		}
	}
}

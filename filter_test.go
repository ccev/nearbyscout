package main

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/expr-lang/expr"
)

func TestCompileFilterRejectsInvalidExpressions(t *testing.T) {
	for _, expression := range []string{
		"", "iv ==", "(iv == 100", "42", "iv", `"true"`,
		"unknown_field == 1", "pokemon_id === 201", `pokemon_id == "201"`,
		"iv_known == 1", "iv_known && cp", "latitude == 0", "pvp.great == 1",
	} {
		t.Run(expression, func(t *testing.T) {
			if _, err := compileFilter(expression); err == nil {
				t.Fatalf("accepted invalid or non-Boolean expression %q", expression)
			}
		})
	}
}

func TestCompileFilterEvaluation(t *testing.T) {
	for _, tc := range []struct {
		expression string
		env        FilterEnv
		want       bool
	}{
		{"true", FilterEnv{}, true},
		{"false", FilterEnv{}, false},
		{"iv_known", FilterEnv{IVKnown: true}, true},
		{"iv_known", FilterEnv{}, false},
		{`(iv_known && iv == 100) || pokemon_id in [201, 25]`, FilterEnv{IVKnown: true, IV: 100}, true},
		{`(iv_known && iv == 100) || pokemon_id in [201, 25]`, FilterEnv{PokemonID: 201}, true},
		{`(iv_known && iv == 100) || pokemon_id in [201, 25]`, FilterEnv{PokemonID: 1, IV: 100}, false},
		{`seen_type == "nearby_stop" && !(pokemon_id in [1, 2])`, FilterEnv{SeenType: "nearby_stop", PokemonID: 25}, true},
		{`seen_type == "nearby_stop" && !(pokemon_id in [1, 2])`, FilterEnv{SeenType: "nearby_stop", PokemonID: 1}, false},
		{"iv == 0", (Pokemon{}).env(), false},
		{"cp < 100", (Pokemon{}).env(), true},
		{"cp >= 0 && cp < 100", (Pokemon{}).env(), false},
		{`individual_attack == 1 && individual_defense == 2 && individual_stamina == 3 && cp == 100 && pokemon_level == 20.5 && form == 4 && costume == 5 && gender == 1 && weather == 2 && great_rank == 3 && ultra_rank == 4`,
			FilterEnv{Attack: 1, Defense: 2, Stamina: 3, CP: 100, Level: 20.5, Form: 4, Costume: 5, Gender: 1, Weather: 2, GreatRank: 3, UltraRank: 4}, true},
	} {
		t.Run(fmt.Sprintf("%s/%v", tc.expression, tc.want), func(t *testing.T) {
			program, err := compileFilter(tc.expression)
			if err != nil {
				t.Fatal(err)
			}
			result, err := expr.Run(program, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := result.(bool)
			if !ok || got != tc.want {
				t.Fatalf("result = %#v (%T), want bool %v", result, result, tc.want)
			}
		})
	}
}

func TestPokemonEnvNumbers(t *testing.T) {
	missing := FilterEnv{IV: -1, Attack: -1, Defense: -1, Stamina: -1, CP: -1, Level: -1,
		Form: -1, Costume: -1, Gender: -1, Weather: -1, GreatRank: math.MaxInt32, UltraRank: math.MaxInt32}
	for _, tc := range []struct {
		name string
		body string
		want FilterEnv
	}{
		{"missing", `{}`, missing},
		{"null", `{"individual_attack":null,"individual_defense":null,"individual_stamina":null,"cp":null,"pokemon_level":null,"form":null,"costume":null,"gender":null,"weather":null}`, missing},
		{"zero", `{"individual_attack":0,"individual_defense":0,"individual_stamina":0,"cp":0,"pokemon_level":0,"form":0,"costume":0,"gender":0,"weather":0}`,
			FilterEnv{IVKnown: true, GreatRank: math.MaxInt32, UltraRank: math.MaxInt32}},
		{"known", `{"pokemon_id":25,"seen_type":"nearby_stop","individual_attack":15,"individual_defense":15,"individual_stamina":15,"cp":123,"pokemon_level":20.5,"form":2,"costume":3,"gender":1,"weather":4}`,
			FilterEnv{PokemonID: 25, SeenType: "nearby_stop", IV: 100, IVKnown: true, Attack: 15, Defense: 15, Stamina: 15, CP: 123, Level: 20.5, Form: 2, Costume: 3, Gender: 1, Weather: 4, GreatRank: math.MaxInt32, UltraRank: math.MaxInt32}},
		{"fractional IV", `{"individual_attack":1,"individual_defense":2,"individual_stamina":4}`,
			FilterEnv{IV: float64(7) * 100 / 45, IVKnown: true, Attack: 1, Defense: 2, Stamina: 4, CP: -1, Level: -1, Form: -1, Costume: -1, Gender: -1, Weather: -1, GreatRank: math.MaxInt32, UltraRank: math.MaxInt32}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p Pokemon
			if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
				t.Fatal(err)
			}
			if got := p.env(); got != tc.want {
				t.Fatalf("env = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPokemonEnvInvalidIVComponents(t *testing.T) {
	for _, field := range []string{"individual_attack", "individual_defense", "individual_stamina"} {
		for _, value := range []string{"null", "-1", "16"} {
			t.Run(field+"="+value, func(t *testing.T) {
				fields := map[string]json.RawMessage{
					"individual_attack": json.RawMessage("15"), "individual_defense": json.RawMessage("15"), "individual_stamina": json.RawMessage("15"),
				}
				fields[field] = json.RawMessage(value)
				data, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				var p Pokemon
				if err := json.Unmarshal(data, &p); err != nil {
					t.Fatal(err)
				}
				e := p.env()
				if e.IVKnown || e.IV != -1 {
					t.Fatalf("invalid IVs treated as known: %+v", e)
				}
				for name, got := range map[string]int{"individual_attack": e.Attack, "individual_defense": e.Defense, "individual_stamina": e.Stamina} {
					want := 15
					if name == field {
						want = -1
						if value == "16" {
							want = 16
						}
					}
					if got != want {
						t.Errorf("%s = %d, want %d", name, got, want)
					}
				}
			})
		}
	}
}

func TestPokemonEnvRanks(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		great, ultra int
	}{
		{"absent", `{}`, math.MaxInt32, math.MaxInt32},
		{"null map", `{"pvp":null}`, math.MaxInt32, math.MaxInt32},
		{"empty map", `{"pvp":{}}`, math.MaxInt32, math.MaxInt32},
		{"null and empty leagues", `{"pvp":{"great":null,"ultra":[]}}`, math.MaxInt32, math.MaxInt32},
		{"no positive ranks", `{"pvp":{"great":[{"rank":0},{"rank":-1},{}],"ultra":[{"rank":-2},{"rank":0}],"master":[{"rank":1}]}}`, math.MaxInt32, math.MaxInt32},
		{"all evolutions and caps", `{"pokemon_id":25,"pvp":{"great":[{"pokemon":25,"cap":40,"rank":50},{"pokemon":26,"cap":50,"rank":2},{"rank":0},{"rank":-1},{"pokemon":25,"cap":51,"rank":10}],"ultra":[{"pokemon":25,"cap":40,"rank":100},{"rank":-5},{"pokemon":26,"cap":51,"rank":3},{"rank":9}],"master":[{"rank":1}]}}`, 2, 3},
		{"last entry minimum", `{"pvp":{"great":[{"rank":5},{"rank":3},{"rank":1}],"ultra":[{"rank":20},{"rank":10},{"rank":4}]}}`, 1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p Pokemon
			if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
				t.Fatal(err)
			}
			e := p.env()
			if e.GreatRank != tc.great || e.UltraRank != tc.ultra {
				t.Fatalf("ranks = %d/%d, want %d/%d", e.GreatRank, e.UltraRank, tc.great, tc.ultra)
			}
		})
	}
}

package main

import (
	"math"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

type Pokemon struct {
	EncounterID   string   `json:"encounter_id"`
	PokemonID     int      `json:"pokemon_id"`
	SeenType      string   `json:"seen_type"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
	DisappearTime int64    `json:"disappear_time"`
	Attack        *int     `json:"individual_attack"`
	Defense       *int     `json:"individual_defense"`
	Stamina       *int     `json:"individual_stamina"`
	CP            *int     `json:"cp"`
	Level         *float64 `json:"pokemon_level"`
	Form          *int     `json:"form"`
	Costume       *int     `json:"costume"`
	Gender        *int     `json:"gender"`
	Weather       *int     `json:"weather"`
	PVP           map[string][]struct {
		Rank int `json:"rank"`
	} `json:"pvp"`
}

type FilterEnv struct {
	PokemonID int     `expr:"pokemon_id"`
	SeenType  string  `expr:"seen_type"`
	IV        float64 `expr:"iv"`
	IVKnown   bool    `expr:"iv_known"`
	Attack    int     `expr:"individual_attack"`
	Defense   int     `expr:"individual_defense"`
	Stamina   int     `expr:"individual_stamina"`
	CP        int     `expr:"cp"`
	Level     float64 `expr:"pokemon_level"`
	Form      int     `expr:"form"`
	Costume   int     `expr:"costume"`
	Gender    int     `expr:"gender"`
	Weather   int     `expr:"weather"`
	GreatRank int     `expr:"great_rank"`
	UltraRank int     `expr:"ultra_rank"`
}

func number[T int | float64](p *T) T {
	if p == nil {
		return -1
	}
	return *p
}

func (p Pokemon) env() FilterEnv {
	e := FilterEnv{PokemonID: p.PokemonID, SeenType: p.SeenType, IV: -1,
		Attack: number(p.Attack), Defense: number(p.Defense), Stamina: number(p.Stamina),
		CP: number(p.CP), Level: number(p.Level), Form: number(p.Form), Costume: number(p.Costume),
		Gender: number(p.Gender), Weather: number(p.Weather), GreatRank: math.MaxInt32, UltraRank: math.MaxInt32}
	e.IVKnown = e.Attack >= 0 && e.Attack <= 15 && e.Defense >= 0 && e.Defense <= 15 && e.Stamina >= 0 && e.Stamina <= 15
	if e.IVKnown {
		e.IV = float64(e.Attack+e.Defense+e.Stamina) * 100 / 45
	}
	for _, rank := range p.PVP["great"] {
		if rank.Rank > 0 && rank.Rank < e.GreatRank {
			e.GreatRank = rank.Rank
		}
	}
	for _, rank := range p.PVP["ultra"] {
		if rank.Rank > 0 && rank.Rank < e.UltraRank {
			e.UltraRank = rank.Rank
		}
	}
	return e
}

func compileFilter(expression string) (*vm.Program, error) {
	return expr.Compile(expression, expr.Env(FilterEnv{}), expr.AsBool())
}

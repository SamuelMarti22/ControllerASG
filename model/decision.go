package model

type Decision string

const (
	Maintain Decision = "MAINTAIN_CAPACITY"
	Increase Decision = "INCREASE_CAPACITY"
	Reduce   Decision = "REDUCE_CAPACITY"
)

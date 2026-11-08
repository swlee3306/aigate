//go:build !race

package pathfold

// raceEnabled is true when the tests are built with -race, whose overhead
// makes wall-clock budgets meaningless.
const raceEnabled = false

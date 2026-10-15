package backtest

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// Data drawn from a known logistic model must give its coefficients back,
// inside the fit's own clustered 95% range.
func TestFitLogitRecoversCoefficients(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	truth := []float64{-0.4, 1.3, 0.7}
	var X [][]float64
	var y []float64
	var cl []string
	for i := 0; i < 20000; i++ {
		x := []float64{1, rng.NormFloat64(), float64(rng.Intn(2))}
		p := sigmoid(dot(x, truth))
		yi := 0.0
		if rng.Float64() < p {
			yi = 1
		}
		X, y, cl = append(X, x), append(y, yi), append(cl, fmt.Sprint(i%200))
	}
	fit, err := fitLogit(X, y, cl)
	if err != nil {
		t.Fatal(err)
	}
	for a, want := range truth {
		if math.Abs(fit.Beta[a]-want) > 1.96*fit.SE[a]+0.01 {
			t.Errorf("β[%d] = %.3f ± %.3f, true %.2f outside", a, fit.Beta[a], 1.96*fit.SE[a], want)
		}
		if fit.SE[a] <= 0 {
			t.Errorf("β[%d] has no standard error", a)
		}
	}
}

func TestInvert(t *testing.T) {
	m := [][]float64{{4, 7}, {2, 6}}
	inv, err := invert(m)
	if err != nil {
		t.Fatal(err)
	}
	id := matMul(m, inv)
	for i := range id {
		for j := range id[i] {
			want := 0.0
			if i == j {
				want = 1
			}
			if math.Abs(id[i][j]-want) > 1e-12 {
				t.Fatalf("m·inv(m) = %v, want identity", id)
			}
		}
	}
	if _, err := invert([][]float64{{1, 2}, {2, 4}}); err == nil {
		t.Error("singular matrix inverted without error")
	}
}

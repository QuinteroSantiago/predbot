package backtest

import (
	"errors"
	"math"
)

// Logistic regression by iteratively reweighted least squares, with a
// cluster-robust (sandwich) covariance. Small and dense: a handful of
// features over a few thousand rows.

// logitFit is a fitted model.
type logitFit struct {
	Beta []float64
	SE   []float64 // clustered; 0 when fewer than two clusters
	Iter int
}

func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

// fitLogit fits y ~ X (X includes any intercept column). cluster[i] groups
// rows whose errors may be correlated.
func fitLogit(X [][]float64, y []float64, cluster []string) (logitFit, error) {
	n := len(X)
	if n == 0 {
		return logitFit{}, errors.New("no rows")
	}
	k := len(X[0])
	beta := make([]float64, k)
	var H [][]float64
	var it int
	for it = 1; it <= 50; it++ {
		grad := make([]float64, k)
		H = zeros(k)
		for i := 0; i < n; i++ {
			p := sigmoid(dot(X[i], beta))
			w := p * (1 - p)
			for a := 0; a < k; a++ {
				grad[a] += X[i][a] * (y[i] - p)
				for b := 0; b < k; b++ {
					H[a][b] += w * X[i][a] * X[i][b]
				}
			}
		}
		Hinv, err := invert(H)
		if err != nil {
			return logitFit{}, err
		}
		step := matVec(Hinv, grad)
		var maxStep float64
		for a := range beta {
			beta[a] += step[a]
			maxStep = math.Max(maxStep, math.Abs(step[a]))
		}
		if maxStep < 1e-10 {
			break
		}
	}
	for a := range beta {
		if math.IsNaN(beta[a]) || math.IsInf(beta[a], 0) {
			return logitFit{}, errors.New("did not converge (perfect separation?)")
		}
	}
	// Recompute the information at the solution for the sandwich.
	H = zeros(k)
	scores := map[string][]float64{}
	for i := 0; i < n; i++ {
		p := sigmoid(dot(X[i], beta))
		w := p * (1 - p)
		s := scores[cluster[i]]
		if s == nil {
			s = make([]float64, k)
			scores[cluster[i]] = s
		}
		for a := 0; a < k; a++ {
			s[a] += X[i][a] * (y[i] - p)
			for b := 0; b < k; b++ {
				H[a][b] += w * X[i][a] * X[i][b]
			}
		}
	}
	Hinv, err := invert(H)
	if err != nil {
		return logitFit{}, err
	}
	fit := logitFit{Beta: beta, SE: make([]float64, k), Iter: it}
	g := float64(len(scores))
	if g >= 2 {
		meat := zeros(k)
		for _, s := range scores {
			for a := 0; a < k; a++ {
				for b := 0; b < k; b++ {
					meat[a][b] += s[a] * s[b]
				}
			}
		}
		V := matMul(matMul(Hinv, meat), Hinv)
		for a := 0; a < k; a++ {
			fit.SE[a] = math.Sqrt(math.Max(V[a][a]*g/(g-1), 0))
		}
	}
	return fit, nil
}

func (f logitFit) predict(x []float64) float64 { return sigmoid(dot(x, f.Beta)) }

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func zeros(k int) [][]float64 {
	m := make([][]float64, k)
	for i := range m {
		m[i] = make([]float64, k)
	}
	return m
}

func matVec(m [][]float64, v []float64) []float64 {
	out := make([]float64, len(m))
	for i := range m {
		out[i] = dot(m[i], v)
	}
	return out
}

func matMul(a, b [][]float64) [][]float64 {
	k := len(a)
	out := zeros(k)
	for i := 0; i < k; i++ {
		for j := 0; j < k; j++ {
			for l := 0; l < k; l++ {
				out[i][j] += a[i][l] * b[l][j]
			}
		}
	}
	return out
}

// invert is Gauss–Jordan with partial pivoting.
func invert(m [][]float64) ([][]float64, error) {
	k := len(m)
	a := make([][]float64, k)
	for i := range m {
		a[i] = make([]float64, 2*k)
		copy(a[i], m[i])
		a[i][k+i] = 1
	}
	for c := 0; c < k; c++ {
		piv := c
		for r := c + 1; r < k; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[piv][c]) {
				piv = r
			}
		}
		if math.Abs(a[piv][c]) < 1e-12 {
			return nil, errors.New("singular matrix (a feature doesn't vary, or two are collinear)")
		}
		a[c], a[piv] = a[piv], a[c]
		d := a[c][c]
		for j := range a[c] {
			a[c][j] /= d
		}
		for r := 0; r < k; r++ {
			if r == c {
				continue
			}
			f := a[r][c]
			for j := range a[r] {
				a[r][j] -= f * a[c][j]
			}
		}
	}
	out := make([][]float64, k)
	for i := range a {
		out[i] = a[i][k:]
	}
	return out, nil
}

package scen

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"text/template"

	"code.google.com/p/go-uuid/uuid"

	_ "github.com/gonum/blas/native"
	"github.com/gonum/matrix/mat64"
	_ "github.com/mxk/go-sqlite/sqlite3"
	"github.com/rwcarlsen/cyan/nuc"
	"github.com/rwcarlsen/cyan/post"
	"github.com/rwcarlsen/cyan/query"
)

// Facility represents a cyclus agent prototype that could be built by the
// optimizer.
type Facility struct {
	Proto string
	// Cap is the total Power output capacity of the facility.
	Cap float64
	// The lifetime of the facility (in timesteps). The lifetime must also
	// be specified manually (consistent with this value) in the prototype
	// definition in the cyclus input template file.
	Life int
	// BuildAfter is the time step after which this facility type can be built.
	// -1 for never available, and 0 for always available.
	BuildAfter int
	// FracOfProto names a prototype that build fractions of this prototype
	// are a portion of.
	FracOfProtos []string
}

// Alive returns whether or not a facility built at the specified time is
// still operating/active at t.
func (f *Facility) Alive(built, t int) bool { return Alive(built, t, f.Life) }

// Available returns true if the facility type can be built at time t.
func (f *Facility) Available(t int) bool {
	return t >= f.BuildAfter && f.BuildAfter >= 0
}

type Build struct {
	Time  int
	Proto string
	N     int
	Life  int
	fac   Facility
}

// Alive returns whether or not the facility is still operabing/active at t.
func (b Build) Alive(t int) bool { return Alive(b.Time, t, b.Lifetime()) }

func (b Build) Lifetime() int {
	if b.Life > 0 {
		return b.Life
	}
	return fac.Life
}

// Alive returns whether or not a facility with the given lifetime and built
// at the specified time is still operating/active at t.
func Alive(built, t, life int) bool {
	return built <= t && (built+life >= t || life <= 0)
}

type Scenario struct {
	// SimDur is the simulation duration in timesteps (months)
	SimDur int
	// BuildOffset is the number of timesteps after simulation start at which
	// deployments actually begin.  This allows facilities and other initial
	// conditions to be set up and run before the deploying begins.
	BuildOffset int
	// TrailingDur is the number of timesteps of the simulation duration that
	// are reserved for wind-down - no new deployments will be made.
	TrailingDur int
	// CyclusTmpl is the path to the text templated cyclus input file.
	CyclusTmpl string
	// BuildPeriod is the number of timesteps between timesteps in which
	// facilities are deployed
	BuildPeriod int
	// NuclideCost represents the waste cost per kg material per time step for
	// each nuclide in the entire simulation (repository's exempt).
	NuclideCost map[string]float64
	// Discount represents the nominal annual discount rate (including
	// inflation) for the simulation.
	Discount float64
	// Facs is a list of facilities that could be built and associated
	// parameters relevant to the optimization objective.
	Facs []Facility
	// MinPower is a series of min deployed power capacity requirements that
	// must be maintained for each build period.
	MinPower []float64
	// MaxPower is a series of max deployed power capacity requirements that
	// must be maintained for each build period.
	MaxPower []float64
	// Builds holds the set of build schedule values for all agents in the
	// scenario.  This can be used to specify initial condition deployments.
	Builds []Build
	// Addr is the location of the cyclus simulation execution server.  An
	// empty string "" indicates that simulations will run locally.
	Addr string
	// File is the name of the scenario file. This is for internal use and
	// does not need to be filled out by the user.
	File string
	// Handle is used internally and does not need to be specified by the
	// user.
	Handle string
}

func (s *Scenario) reactors() []Facility {
	rs := []Facility{}
	for _, fac := range s.Facs {
		if fac.Cap > 0 {
			rs = append(rs, fac)
		}
	}
	return rs
}

func (s *Scenario) notreactors() []Facility {
	fs := []Facility{}
	for _, fac := range s.Facs {
		if fac.Cap == 0 {
			fs = append(fs, fac)
		}
	}
	return fs
}

func (s *Scenario) nvars() int { return s.nvarsPerPeriod * s.nperiods() }

func (s *Scenario) nvarsPerPeriod() int {
	numFacVars := len(s.Facs) - 1
	numPowerVars := 1
	return numFacVars + numPowerVars
}

func (s *Scenario) periodFacOrder() (varfacs []Facility, implicitreactor Facility) {
	facs := []Facility{}
	for i, fac := range s.reactors()[1:] {
		facs = append(facs, fac)
	}
	for i, fac := range s.notreactors() {
		facs = append(facs, fac)
	}
	return facs
}

// TransformVars takes a sequence of input variables for the scenario and
// transforms them into a set of prototype/facility deployments. The sequence
// of the vars follows this pattern: fac1_t1, fac1_t2, ..., fac1_tn, fac2_t1,
// ..., facm_t1, facm_t2, ..., facm_tn.
//
// The first reactor type variable represents the total fraction of new built
// power capacity satisfied by that reactor on the given time step.  For each
// subsequent reactor type (except the last), the variables represent the
// fraction of the remaining power capacity satisfied by that reactor type
// (e.g. the third reactor type's variable can be used to calculate its
// fraction like this (1-(react1frac + (1-react1frac) * react2frac)) *
// react3frac).  The last reactor type fraction is simply the remainining
// unsatisfied power capacity.
func (s *Scenario) TransformVars(vars []float64) (map[string][]Build, error) {
	err := s.Validate()
	if err != nil {
		return nil, err
	} else if len(vars) != s.nvars() {
		return nil, fmt.Errorf("wrong number of vars: want %v, got %v", s.nvars(), len(vars))
	}

	builds := map[string][]Build{}
	for _, b := range s.Builds {
		builds[b.Proto] = b
	}

	varfacs, implicitreactor := s.periodFacOrder()
	caperror := map[string]float64{}
	for i, t := range s.periodTimes() {
		minpow := s.MinPower[t]
		maxpow := s.MaxPower[t]
		currpower := s.powercap(builds, t)
		powervar := vars[s.BuildPeriod*i]
		captobuild := math.Max(minpow-currpower, 0)
		powerrange := maxpow - (currpower + captobuild)
		captobuild += powervar * powerrange
		finalpower := currpower + captobuild

		// handle reactor builds
		reactorfrac := 0.0
		j := 1
		for j = 1; j < s.nvarsPerPeriod(); j++ {
			val := vars[s.BuildPeriod*i+j]
			fac := varfacs[j]
			if fac.Cap > 0 {
				facfrac := (1 - reactorfrac) * val
				reactorfrac += facfrac

				caperr := caperror[fac.Proto]
				wantcap := facfrac*captobuild + caperr
				nbuild := int(math.Max(math.Floor(wantcap/fac.Cap + 0.5)))
				caperror[fac.Proto] = wantcap - float64(nbuild)*fac.Cap

				builds[fac.Proto] = append(builds[fac.Proto], Build{
					Time:  t,
					Proto: fac.Proto,
					N:     nbuild,
					fac:   fac,
				})
			} else {
				break
			}
		}

		// handle implicit reactor
		j := 0
		fac := implicitreactor
		facfrac := (1 - reactorfrac)

		caperr := caperror[fac.Proto]
		wantcap := facfrac*captobuild + caperr
		nbuild := int(math.Max(0, math.Floor(wantcap/fac.Cap+0.5)))
		caperror[fac.Proto] = wantcap - float64(nbuild)*fac.Cap

		builds[fac.Proto] = append(builds[fac.Proto], Build{
			Time:  t,
			Proto: fac.Proto,
			N:     nbuild,
			fac:   fac,
		})

		// handle other facilities
		for ; j < s.nvarsPerPeriod(); j++ {
			facfrac := vars[s.BuildPeriod*i+j]
			fac := varfacs[j]

			caperr := caperror[fac.Proto]
			wantn := facfrac*float64(s.naliveproto(builds, t, fac.FracOfProtos...)) + caperr
			nbuild := int(math.Max(0, math.Floor(float64(wantn)+0.5)))
			caperror[fac.Proto] = wantcap - float64(nbuild)

			builds[fac.Proto] = append(builds[fac.Proto], Build{
				Time:  t,
				Proto: fac.Proto,
				N:     nbuild,
				fac:   fac,
			})
		}
	}

	return builds, nil
}

func (s *Scenario) naliveproto(facs map[string][]Build, t int, protos ...string) int {
	count := 0
	for _, proto := range protos {
		builds := facs[proto]
		for _, b := range builds {
			if b.Alive(t) {
				count++
			}
		}
	}
	return count
}

func (s *Scenario) powercap(builds map[string][]Build, t int) float64 {
	pow := 0.0
	for _, buildsproto := range builds {
		for _, b := range buildsproto {
			if b.Alive(t) {
				pow += fac.Cap * b.N
			}
		}
	}
	return pow
}

// Validate returns an error if the scenario is ill-configured.
func (s *Scenario) Validate() error {
	if min, max := len(s.MinPower), len(s.MaxPower); min != max {
		return fmt.Errorf("MaxPower length %v != MinPower length %v", max, min)
	}

	np := s.nperiods()
	lmin := len(s.MinPower)
	if np != lmin {
		return fmt.Errorf("number power constraints %v != number build periods %v", lmin, np)
	}

	protos := map[string]bool{}
	for _, fac := range s.Facs {
		protos[fac.Proto] = fac
	}

	for _, p := range s.Builds {
		if fac, ok := protos[p.Proto]; !ok {
			return fmt.Errorf("param prototype '%v' is not defined in Facs", p.Proto)
		}
		p.fac = fac
	}

	return nil
}

func (s *Scenario) Load(fname string) error {
	if s == nil {
		s = &Scenario{}
	}
	data, err := ioutil.ReadFile(fname)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, s); err != nil {
		if serr, ok := err.(*json.SyntaxError); ok {
			line, col := findLine(data, serr.Offset)
			return fmt.Errorf("%s:%d:%d: %v", fname, line, col, err)
		}
		return err
	}

	s.File = fname
	if len(s.Builds) == 0 {
		s.Builds = make([]Build, s.nvars())
	}
	return s.Validate()
}

func (s *Scenario) GenCyclusInfile() ([]byte, error) {
	if s.Handle == "" {
		s.Handle = "none"
	}

	var buf bytes.Buffer
	tmpl := s.CyclusTmpl
	t := template.Must(template.ParseFiles(tmpl))

	err := t.Execute(&buf, s)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (s *Scenario) Run(stdout, stderr io.Writer) (dbfile string, simid []byte, err error) {
	// generate cyclus input file and run cyclus
	ui := uuid.NewRandom()
	cycin := ui.String() + ".cyclus.xml"
	cycout := ui.String() + ".sqlite"

	data, err := s.GenCyclusInfile()
	if err != nil {
		return "", nil, err
	}
	err = ioutil.WriteFile(cycin, data, 0644)
	if err != nil {
		return "", nil, err
	}

	cmd := exec.Command("cyclus", cycin, "-o", cycout)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}

	if err := cmd.Run(); err != nil {
		return "", nil, err
	}

	// post process cyclus output db
	db, err := sql.Open("sqlite3", cycout)
	if err != nil {
		return "", nil, err
	}
	defer db.Close()

	simids, err := post.Process(db)
	if err != nil {
		return "", nil, err
	}

	return cycout, simids[0], nil
}

func (s *Scenario) VarNames() []string {
	nperiods := s.nperiods()
	names := make([]string, s.nvars())
	for f := range s.Facs {
		for n, t := range s.periodTimes() {
			i := f*nperiods + n
			names[i] = fmt.Sprintf("f%v_t%v", f, t)
		}
	}
	return names
}

func (s *Scenario) LowerBounds() *mat64.Dense {
	return mat64.NewDense(s.nvars(), 1, nil)
}

func (s *Scenario) UpperBounds() *mat64.Dense {
	nperiods := s.nperiods()
	up := mat64.NewDense(s.nvars(), 1, nil)
	for f, fac := range s.Facs {
		for n, t := range s.periodTimes() {
			row := f*nperiods + n
			if !fac.Available(t) {
				up.Set(row, 0, 0)
			} else if fac.Cap != 0 {
				v := s.MaxPower[n]/fac.Cap*.2 + 1
				if v < 10 {
					v = 10
				}
				for _, ifac := range s.Builds {
					if ifac.Proto == fac.Proto && Alive(ifac.Time, t, fac.Life) {
						v -= float64(ifac.N)
					}
				}
				if v < 0 {
					v = 0
				}
				up.Set(row, 0, v)
			} else {
				up.Set(row, 0, 10)
			}
		}
	}
	return up
}

func (s *Scenario) timeOf(period int) int {
	return period*s.BuildPeriod + 1 + s.BuildOffset
}

func (s *Scenario) periodOf(time int) int {
	return (time - s.BuildOffset - 1) / s.BuildPeriod
}

func (s *Scenario) periodTimes() []int {
	periods := make([]int, s.nperiods())
	for i := range periods {
		periods[i] = s.timeOf(i)
	}
	return periods
}

func (s *Scenario) nperiods() int {
	return (s.SimDur-s.BuildOffset-s.TrailingDur-2)/s.BuildPeriod + 1
}

func findLine(data []byte, pos int64) (line, col int) {
	line = 1
	buf := bytes.NewBuffer(data)
	for n := int64(0); n < pos; n++ {
		b, err := buf.ReadByte()
		if err != nil {
			panic(err) //I don't really see how this could happen
		}
		if b == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return
}

func (scen *Scenario) CalcObjective(dbfile string, simid []byte) (float64, error) {
	db, err := sql.Open("sqlite3", dbfile)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	// add up overnight and operating costs converted to PV(t=0)
	q1 := `
		SELECT tl.Time FROM TimeList AS tl
			INNER JOIN Agents As a ON a.EnterTime <= tl.Time AND (a.ExitTime >= tl.Time OR a.ExitTime IS NULL)
		WHERE
			a.SimId = tl.SimId AND a.SimId = ?
			AND a.Prototype = ?;
		`
	q2 := `SELECT EnterTime FROM Agents WHERE SimId = ? AND Prototype = ?`

	totcost := 0.0
	for _, fac := range scen.Facs {
		// calc total operating cost
		rows, err := db.Query(q1, simid, fac.Proto)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var t int
			if err := rows.Scan(&t); err != nil {
				return 0, err
			}
			totcost += PV(fac.OpCost, t, scen.Discount)
		}
		if err := rows.Err(); err != nil {
			return 0, err
		}

		// calc overnight capital cost
		rows, err = db.Query(q2, simid, fac.Proto)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var t int
			if err := rows.Scan(&t); err != nil {
				return 0, err
			}
			totcost += PV(fac.CapitalCost, t, scen.Discount)
		}
		if err := rows.Err(); err != nil {
			return 0, err
		}

		// add in waste penalty
		ags, err := query.AllAgents(db, simid, fac.Proto)
		if err != nil {
			return 0, err
		}

		// InvAt uses all agents if no ids are passed - so we need to skip from here
		if len(ags) == 0 {
			continue
		}

		ids := make([]int, len(ags))
		for i, a := range ags {
			ids[i] = a.Id
		}

		for t := 0; t < scen.SimDur; t++ {
			mat, err := query.InvAt(db, simid, t, ids...)
			if err != nil {
				return 0, err
			}
			for nuc, qty := range mat {
				nucstr := fmt.Sprint(nuc)
				totcost += PV(scen.NuclideCost[nucstr]*float64(qty)*(1-fac.WasteDiscount), t, scen.Discount)
			}
		}
	}

	// normalize to energy produced
	joules, err := query.EnergyProduced(db, simid, 0, scen.SimDur)
	if err != nil {
		return 0, err
	}
	mwh := joules / nuc.MWh
	mult := 1e6 // to get the objective around 0.1 same magnitude as constraint penalties
	return totcost / (mwh + 1e-30) * mult, nil
}

func PV(amt float64, nt int, rate float64) float64 {
	monrate := rate / 12
	return amt / math.Pow(1+monrate, float64(nt))
}

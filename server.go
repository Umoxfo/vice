// server.go
// Copyright(c) 2023 Matt Pharr, licensed under the GNU Public License, Version 3.
// SPDX: GPL-3.0-only

package main

import (
	crand "crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/mmp/imgui-go/v4"
)

var (
	ErrControllerAlreadySignedIn = errors.New("controller with that callsign already signed in")
	ErrInvalidControllerToken    = errors.New("invalid controller token")
)

/*
TODO:
***   maybe Sim is really something like World and is purely read-only data for stars, etc?
 then Server becomes Sim, it handles all changes to the world.
 need to move time management, lots of other stuff to server
 then things like server rate need to come through the api...
  - automatic handoffs, etc should be handled here.

- prespawn not working?
- don't want eventsId allocated for the Sim inside Server...

move to server(?)
	ScenarioGroupName string
	ScenarioName      string

- catch errors early, on client side, when possible (though server is canonical for e.g. who is the tracking controller)
- Wind...
- make sure not using sim.Callsign in this file!!!!
- think about serialization for restart...
- stop holding *Aircraft and assuming callsign->*Aircraft will be
  consistent (mostly an issue in STARSPane); just use callsign?
  -> this is necessary for events when *Aircraft pointers are different
- drop controller if no messages for some period of time
- is a mutex needed? how is concurrency handled by net/rpc?
- stars contorller list should be updated based on who is signed in
- think through serialize/deserialize of Server
  need to rewriet Handoff times
*/

type Server struct {
	Sim         *Sim
	controllers map[string]*ServerController // from token

	showSettings bool

	SerializeTime time.Time // for updating times on deserialize

	// airport -> runway -> category -> rate
	DepartureRates map[string]map[string]map[string]*int32
	// arrival group -> airport -> rate
	ArrivalGroupRates map[string]map[string]*int32

	// The same runway may be present multiple times in DepartureRates,
	// with different categories. However, we want to make sure that we
	// don't spawn two aircraft on the same runway at the same time (or
	// close to it).  Therefore, here we track a per-runway "when's the
	// next time that we will spawn *something* from the runway" time.
	// When the time is up, we'll figure out which specific category to
	// use...
	// airport -> runway -> time
	NextDepartureSpawn map[string]map[string]time.Time

	// Key is arrival group name
	NextArrivalSpawn map[string]time.Time

	Handoffs map[string]time.Time

	DepartureChallenge float32
	GoAroundRate       float32

	lastTrackUpdate time.Time
	lastSimUpdate   time.Time
	eventsId        EventSubscriberId

	currentTime    time.Time // this is our fake time--accounting for pauses & simRate..
	lastUpdateTime time.Time // this is w.r.t. true wallclock time
	SimRate        float32
	Paused         bool
}

type ServerController struct {
	Callsign string
	EventsId EventSubscriberId
	// *net.Conn?
}

func NewServer(ssc NewSimConfiguration) *Server {
	rand.Seed(time.Now().UnixNano())

	s := &Server{
		controllers: make(map[string]*ServerController),

		DepartureRates:    DuplicateMap(ssc.departureRates),
		ArrivalGroupRates: DuplicateMap(ssc.arrivalGroupRates),

		currentTime:    time.Now(),
		lastUpdateTime: time.Now(),
		eventsId:       eventStream.Subscribe(),

		SimRate:            1,
		DepartureChallenge: ssc.departureChallenge,
		GoAroundRate:       ssc.goAroundRate,
		Handoffs:           make(map[string]time.Time),
	}

	s.Sim = newSim(ssc, s)

	s.setInitialSpawnTimes()
	s.prespawn()

	return s
}

func newSim(ssc NewSimConfiguration, s *Server) *Sim {
	sim := &Sim{
		ScenarioGroupName: ssc.scenarioGroup.Name,
		ScenarioName:      ssc.scenario.Name(),

		Callsign:          "__SERVER__",
		Wind:              ssc.scenario.Wind,
		MagneticVariation: ssc.scenarioGroup.MagneticVariation,
		NmPerLatitude:     ssc.scenarioGroup.NmPerLatitude,
		NmPerLongitude:    ssc.scenarioGroup.NmPerLongitude,
		Airports:          ssc.scenarioGroup.Airports,
		Fixes:             ssc.scenarioGroup.Fixes,
		PrimaryAirport:    ssc.scenarioGroup.PrimaryAirport,
		RadarSites:        ssc.scenarioGroup.RadarSites,
		Center:            ssc.scenarioGroup.Center,
		Range:             ssc.scenarioGroup.Range,
		STARSMaps:         ssc.scenarioGroup.STARSMaps,
		Scratchpads:       ssc.scenarioGroup.Scratchpads,
		ArrivalGroups:     ssc.scenarioGroup.ArrivalGroups,
		ApproachAirspace:  ssc.scenario.ApproachAirspace,
		DepartureAirspace: ssc.scenario.DepartureAirspace,
		DepartureRunways:  ssc.scenario.DepartureRunways,

		Aircraft: make(map[string]*Aircraft),
		METAR:    make(map[string]*METAR),

		eventsId: eventStream.Subscribe(),
	}

	sim.Controllers = make(map[string]*Controller)
	// Extract just the active controllers
	for callsign, ctrl := range ssc.scenarioGroup.ControlPositions {
		if Find(ssc.scenario.Controllers, callsign) != -1 {
			sim.Controllers[callsign] = ctrl
		}
	}

	// Make some fake METARs; slightly different for all airports.
	alt := 2980 + rand.Intn(40)
	fakeMETAR := func(icao string) {
		spd := sim.Wind.Speed - 3 + rand.Int31n(6)
		var wind string
		if spd < 0 {
			wind = "00000KT"
		} else if spd < 4 {
			wind = fmt.Sprintf("VRB%02dKT", spd)
		} else {
			dir := 10 * ((sim.Wind.Direction + 5) / 10)
			dir += [3]int32{-10, 0, 10}[rand.Intn(3)]
			wind = fmt.Sprintf("%03d%02d", dir, spd)
			gst := sim.Wind.Gust - 3 + rand.Int31n(6)
			if gst-sim.Wind.Speed > 5 {
				wind += fmt.Sprintf("G%02d", gst)
			}
			wind += "KT"
		}

		// Just provide the stuff that the STARS display shows
		sim.METAR[icao] = &METAR{
			AirportICAO: icao,
			Wind:        wind,
			Altimeter:   fmt.Sprintf("A%d", alt-2+rand.Intn(4)),
		}
	}

	sim.DepartureAirports = make(map[string]*Airport)
	for name, runwayRates := range s.DepartureRates {
		for _, categoryRates := range runwayRates {
			for _, rate := range categoryRates {
				if *rate > 0 {
					sim.DepartureAirports[name] = sim.GetAirport(name)
				}
			}
		}
	}
	sim.ArrivalAirports = make(map[string]*Airport)
	for _, airportRates := range s.ArrivalGroupRates {
		for name, rate := range airportRates {
			if *rate > 0 {
				sim.ArrivalAirports[name] = sim.GetAirport(name)
			}
		}
	}

	for ap := range sim.DepartureAirports {
		fakeMETAR(ap)
	}
	for ap := range sim.ArrivalAirports {
		fakeMETAR(ap)
	}

	return sim
}

func (s *Server) SignOn(callsign string) (*Sim, error) {
	for _, ctrl := range s.controllers {
		if ctrl.Callsign == callsign {
			return nil, ErrControllerAlreadySignedIn
		}
	}

	sim := &Sim{}
	*sim = *s.Sim
	sim.Callsign = callsign

	var buf [16]byte
	if _, err := crand.Read(buf[:]); err != nil {
		return nil, err
	}

	sim.token = base64.StdEncoding.EncodeToString(buf[:])
	s.controllers[sim.token] = &ServerController{
		Callsign: callsign,
		EventsId: eventStream.Subscribe(),
	}

	return sim, nil
}

func (s *Server) SignOff(token string, _ *struct{}) error {
	delete(s.controllers, token)
	return nil
}

func (s *Server) IsPaused() bool {
	return s.Paused
}

func (s *Server) TogglePause() {
	s.Paused = !s.Paused
	s.lastUpdateTime = time.Now() // ignore time passage...
}

func (s *Server) CurrentTime() time.Time {
	return s.currentTime
}

func (s *Server) GetWindVector(p Point2LL, alt float32) Point2LL {
	// Sinusoidal wind speed variation from the base speed up to base +
	// gust and then back...
	base := time.UnixMicro(0)
	sec := s.currentTime.Sub(base).Seconds()
	windSpeed := float32(s.Sim.Wind.Speed) +
		float32(s.Sim.Wind.Gust)*float32(1+math.Cos(sec/4))/2

	// Wind.Direction is where it's coming from, so +180 to get the vector
	// that affects the aircraft's course.
	d := OppositeHeading(float32(s.Sim.Wind.Direction))
	vWind := [2]float32{sin(radians(d)), cos(radians(d))}
	vWind = scale2f(vWind, windSpeed/3600)
	return vWind
}

func (s *Server) Activate() error {
	var e ErrorLogger

	s.controllers = make(map[string]*ServerController)

	initializeWaypointLocations := func(waypoints []Waypoint, e *ErrorLogger) {
		for i, wp := range waypoints {
			if e != nil {
				e.Push("Fix " + wp.Fix)
			}
			if pos, ok := sim.Locate(wp.Fix); !ok {
				if e != nil {
					e.ErrorString("unable to locate waypoint")
				}
			} else {
				waypoints[i].Location = pos
			}
			if e != nil {
				e.Pop()
			}
		}
	}

	now := time.Now()
	s.currentTime = now
	s.lastUpdateTime = now
	s.eventsId = eventStream.Subscribe()

	// A number of time.Time values are included in the serialized Sim.
	// updateTime is a helper function that rewrites them to be in terms of
	// the current time, using the serializion time as a baseline.
	updateTime := func(t time.Time) time.Time {
		return now.Add(t.Sub(s.SerializeTime))
	}

	for _, ac := range s.Sim.Aircraft {
		e.Push(ac.Callsign)

		// Rewrite the radar track times to be w.r.t now
		for i := range ac.Tracks {
			ac.Tracks[i].Time = updateTime(ac.Tracks[i].Time)
		}

		if ap := ac.Approach(); ap != nil {
			for i := range ap.Waypoints {
				initializeWaypointLocations(ap.Waypoints[i], &e)
			}
		}

		for rwy, wp := range ac.ArrivalRunwayWaypoints {
			e.Push("Arrival runway " + rwy)
			initializeWaypointLocations(wp, &e)
			e.Pop()
		}

		e.Pop()
		eventStream.Post(&AddedAircraftEvent{ac: ac})
	}

	for callsign := range sim.Controllers {
		sim.Controllers[callsign].Callsign = callsign
	}

	sg := scenarioGroups[sim.ScenarioGroupName]

	if sg == nil {
		e.ErrorString(sim.ScenarioGroupName + ": unknown scenario group")
	} else {
		if len(sg.STARSMaps) != len(sim.STARSMaps) {
			e.ErrorString("Different number of STARSMaps in ScenarioGroup and Saved sim")
		} else {
			for i := range sim.STARSMaps {
				if sg.STARSMaps[i].Name != sim.STARSMaps[i].Name {
					e.ErrorString("Name mismatch in STARSMaps: ScenarioGroup \"" + sg.STARSMaps[i].Name +
						"\", Sim \"" + sim.STARSMaps[i].Name + "\"")
				} else {
					// Copy the command buffer so we can draw the thing...
					sim.STARSMaps[i].cb = sg.STARSMaps[i].cb
				}
			}
		}
	}

	for i, rwy := range sim.DepartureRunways {
		s.Sim.DepartureRunways[i].lastDeparture = nil
		for _, route := range rwy.ExitRoutes {
			initializeWaypointLocations(route.Waypoints, &e)
		}
	}

	for _, arrivals := range sim.ArrivalGroups {
		for _, arr := range arrivals {
			initializeWaypointLocations(arr.Waypoints, &e)
			for _, rwp := range arr.RunwayWaypoints {
				initializeWaypointLocations(rwp, &e)
			}
		}
	}

	for ho, t := range s.Handoffs {
		s.Handoffs[ho] = updateTime(t)
	}

	for group, t := range s.NextArrivalSpawn {
		s.NextArrivalSpawn[group] = updateTime(t)
	}

	for airport, runwayTimes := range s.NextDepartureSpawn {
		for runway, t := range runwayTimes {
			s.NextDepartureSpawn[airport][runway] = updateTime(t)
		}
	}

	if e.HaveErrors() {
		e.PrintErrors()
		return errors.New("Errors during state restoration")
	}
	return nil

}

///////////////////////////////////////////////////////////////////////////
// Settings

func (s *Server) ToggleActivateSettingsWindow() {
	s.showSettings = !s.showSettings
}

func (s *Server) DrawSettingsWindow() {
	if !s.showSettings {
		return
	}

	imgui.BeginV("Simulation Settings", &s.showSettings, imgui.WindowFlagsAlwaysAutoResize)

	if *devmode {
		imgui.SliderFloatV("Simulation speed", &s.SimRate, 1, 100, "%.1f", 0)
	} else {
		imgui.SliderFloatV("Simulation speed", &s.SimRate, 1, 10, "%.1f", 0)
	}

	if imgui.BeginComboV("UI Font Size", fmt.Sprintf("%d", globalConfig.UIFontSize), imgui.ComboFlagsHeightLarge) {
		sizes := make(map[int]interface{})
		for fontid := range fonts {
			if fontid.Name == "Roboto Regular" {
				sizes[fontid.Size] = nil
			}
		}
		for _, size := range SortedMapKeys(sizes) {
			if imgui.SelectableV(fmt.Sprintf("%d", size), size == globalConfig.UIFontSize, 0, imgui.Vec2{}) {
				globalConfig.UIFontSize = size
				ui.font = GetFont(FontIdentifier{Name: "Roboto Regular", Size: globalConfig.UIFontSize})
			}
		}
		imgui.EndCombo()
	}
	if imgui.BeginComboV("STARS DCB Font Size", fmt.Sprintf("%d", globalConfig.DCBFontSize), imgui.ComboFlagsHeightLarge) {
		sizes := make(map[int]interface{})
		for fontid := range fonts {
			if fontid.Name == "Inconsolata Condensed Regular" {
				sizes[fontid.Size] = nil
			}
		}
		for _, size := range SortedMapKeys(sizes) {
			if imgui.SelectableV(fmt.Sprintf("%d", size), size == globalConfig.DCBFontSize, 0, imgui.Vec2{}) {
				globalConfig.DCBFontSize = size
			}
		}
		imgui.EndCombo()
	}

	var fsp *FlightStripPane
	var stars *STARSPane
	globalConfig.DisplayRoot.VisitPanes(func(p Pane) {
		switch pane := p.(type) {
		case *FlightStripPane:
			fsp = pane
		case *STARSPane:
			stars = pane
		}
	})

	stars.DrawUI()

	imgui.Separator()

	if imgui.CollapsingHeader("Audio") {
		globalConfig.Audio.DrawUI()
	}
	if fsp != nil && imgui.CollapsingHeader("Flight Strips") {
		fsp.DrawUI()
	}
	if imgui.CollapsingHeader("Developer") {
		if imgui.BeginTableV("GlobalFiles", 4, 0, imgui.Vec2{}, 0) {
			imgui.TableNextRow()
			imgui.TableNextColumn()
			imgui.Text("Scenario:")
			imgui.TableNextColumn()
			imgui.Text(globalConfig.DevScenarioFile)
			imgui.TableNextColumn()
			if imgui.Button("New...##scenario") {
				ui.jsonSelectDialog = NewFileSelectDialogBox("Select JSON File", []string{".json"},
					globalConfig.DevScenarioFile, func(filename string) {
						globalConfig.DevScenarioFile = filename
						ui.jsonSelectDialog = nil
					})
				ui.jsonSelectDialog.Activate()
			}
			imgui.TableNextColumn()
			if globalConfig.DevScenarioFile != "" && imgui.Button("Clear##scenario") {
				globalConfig.DevScenarioFile = ""
			}

			imgui.TableNextRow()
			imgui.TableNextColumn()
			imgui.Text("Video maps:")
			imgui.TableNextColumn()
			imgui.Text(globalConfig.DevVideoMapFile)
			imgui.TableNextColumn()
			if imgui.Button("New...##vid") {
				ui.jsonSelectDialog = NewFileSelectDialogBox("Select JSON File", []string{".json"},
					globalConfig.DevVideoMapFile, func(filename string) {
						globalConfig.DevVideoMapFile = filename
						ui.jsonSelectDialog = nil
					})
				ui.jsonSelectDialog.Activate()
			}
			imgui.TableNextColumn()
			if globalConfig.DevVideoMapFile != "" && imgui.Button("Clear##vid") {
				globalConfig.DevVideoMapFile = ""
			}

			imgui.EndTable()
		}

		if ui.jsonSelectDialog != nil {
			ui.jsonSelectDialog.Draw()
		}
	}

	imgui.End()
}

///////////////////////////////////////////////////////////////////////////
// Simulation

func (s *Server) Update() {
	if s.Paused {
		return
	}

	// Process events
	if s.eventsId != InvalidEventSubscriberId {
		for _, ev := range eventStream.Get(s.eventsId) {
			if rem, ok := ev.(*RemovedAircraftEvent); ok {
				delete(s.Sim.Aircraft, rem.ac.Callsign)
			}
			if ack, ok := ev.(*AckedHandoffEvent); ok {
				// the user acknowledged that the other controller took the
				// handoff. This is the point where the other controller
				// takes control.  We'll just climb them to their cruise
				// altitude...
				if ack.ac.IsDeparture {
					lg.Errorf("%s: climbing to %d", ack.ac.Callsign, ack.ac.FlightPlan.Altitude)
					ack.ac.Nav.V = &MaintainAltitude{
						Altitude: float32(ack.ac.FlightPlan.Altitude),
					}
				}
			}
		}
	}

	// Update the current time
	elapsed := time.Since(s.lastUpdateTime)
	elapsed = time.Duration(s.SimRate * float32(elapsed))
	s.currentTime = s.currentTime.Add(elapsed)
	s.lastUpdateTime = time.Now()

	s.updateState()
}

// separate so time management can be outside this so we can do the prespawn stuff...
func (s *Server) updateState() {
	now := s.currentTime
	for callsign, t := range s.Handoffs {
		if now.After(t) {
			if ac, ok := s.Sim.Aircraft[callsign]; ok {
				ac.TrackingController = ac.OutboundHandoffController
				ac.OutboundHandoffController = ""
				eventStream.Post(&AcceptedHandoffEvent{controller: ac.TrackingController, ac: ac})
				globalConfig.Audio.PlaySound(AudioEventHandoffAccepted)
			}
			delete(s.Handoffs, callsign)
		}
	}

	// Update the simulation state once a second.
	if now.Sub(s.lastSimUpdate) >= time.Second {
		s.lastSimUpdate = now
		for _, ac := range s.Sim.Aircraft {
			ac.Update()
		}
	}

	// Add a new radar track every 5 seconds.  While we're at it, cull
	// departures that are far from the airport.
	if now.Sub(s.lastTrackUpdate) >= 5*time.Second {
		s.lastTrackUpdate = now

		for callsign, ac := range s.Sim.Aircraft {
			if ap := s.Sim.GetAirport(ac.FlightPlan.DepartureAirport); ap != nil && ac.IsDeparture {
				if nmdistance2ll(ac.Position, ap.Location) > 200 {
					eventStream.Post(&RemovedAircraftEvent{ac: ac})
					delete(s.Sim.Aircraft, callsign)
					continue
				}
			}

			ac.AddTrack(RadarTrack{
				Position:    ac.Position,
				Altitude:    int(ac.Altitude),
				Groundspeed: int(ac.GS),
				Heading:     ac.Heading - s.Sim.MagneticVariation,
				Time:        now,
			})

			eventStream.Post(&ModifiedAircraftEvent{ac: ac})
		}
	}

	s.spawnAircraft()
}

func (s *Server) prespawn() {
	// Prime the pump before the user gets involved
	t := time.Now().Add(-(initialSimSeconds + 1) * time.Second)
	for i := 0; i < initialSimSeconds; i++ {
		s.currentTime = t
		s.lastUpdateTime = t
		t = t.Add(1 * time.Second)

		s.updateState()
	}
	s.currentTime = time.Now()
	s.lastUpdateTime = time.Now()
}

///////////////////////////////////////////////////////////////////////////
// Spawning aircraft

func (s *Server) setInitialSpawnTimes() {
	// Randomize next spawn time for departures and arrivals; may be before
	// or after the current time.
	randomSpawn := func(rate int) time.Time {
		if rate == 0 {
			return time.Now().Add(365 * 24 * time.Hour)
		}
		avgWait := 3600 / rate
		delta := rand.Intn(avgWait) - avgWait/2 - initialSimSeconds
		return time.Now().Add(time.Duration(delta) * time.Second)
	}

	s.NextArrivalSpawn = make(map[string]time.Time)
	for group, rates := range s.ArrivalGroupRates {
		rateSum := 0
		for _, rate := range rates {
			rateSum += int(*rate)
		}
		s.NextArrivalSpawn[group] = randomSpawn(rateSum)
	}

	s.NextDepartureSpawn = make(map[string]map[string]time.Time)
	for airport, runwayRates := range s.DepartureRates {
		spawn := make(map[string]time.Time)

		for runway, categoryRates := range runwayRates {
			rateSum := 0
			for _, rate := range categoryRates {
				rateSum += int(*rate)
			}
			if rateSum > 0 {
				spawn[runway] = randomSpawn(rateSum)
			}
		}

		if len(spawn) > 0 {
			s.NextDepartureSpawn[airport] = spawn
		}
	}
}

func sampleRateMap(rates map[string]*int32) (string, int) {
	// Choose randomly in proportion to the rates in the map
	rateSum := 0
	var result string
	for item, rate := range rates {
		rateSum += int(*rate)
		// Weighted reservoir sampling...
		if rand.Float32() < float32(int(*rate))/float32(rateSum) {
			result = item
		}
	}
	return result, rateSum
}

func (s *Server) spawnAircraft() {
	now := sim.CurrentTime()

	addAircraft := func(ac *Aircraft) {
		if _, ok := sim.Aircraft[ac.Callsign]; ok {
			lg.Errorf("%s: already have an aircraft with that callsign!", ac.Callsign)
			return
		}
		sim.Aircraft[ac.Callsign] = ac

		ac.RunWaypointCommands(ac.Waypoints[0])

		ac.Position = ac.Waypoints[0].Location
		if ac.Position.IsZero() {
			lg.Errorf("%s: uninitialized initial waypoint position! %+v", ac.Callsign, ac.Waypoints[0])
			return
		}

		ac.Heading = float32(ac.Waypoints[0].Heading)
		if ac.Heading == 0 { // unassigned, so get the heading from the next fix
			ac.Heading = headingp2ll(ac.Position, ac.Waypoints[1].Location, sim.MagneticVariation)
		}
		ac.Waypoints = FilterSlice(ac.Waypoints[1:], func(wp Waypoint) bool { return !wp.Location.IsZero() })

		eventStream.Post(&AddedAircraftEvent{ac: ac})
	}

	randomWait := func(rate int) time.Duration {
		if rate == 0 {
			return 365 * 24 * time.Hour
		}
		avgSeconds := 3600 / float32(rate)
		seconds := lerp(rand.Float32(), .85*avgSeconds, 1.15*avgSeconds)
		return time.Duration(seconds * float32(time.Second))
	}

	for group, airportRates := range s.ArrivalGroupRates {
		if now.After(s.NextArrivalSpawn[group]) {
			arrivalAirport, rateSum := sampleRateMap(airportRates)

			if ac := s.SpawnArrival(arrivalAirport, group); ac != nil {
				ac.FlightPlan.ArrivalAirport = arrivalAirport
				addAircraft(ac)
				s.NextArrivalSpawn[group] = now.Add(randomWait(rateSum))
			}
		}
	}

	for airport, runwayTimes := range s.NextDepartureSpawn {
		for runway, spawnTime := range runwayTimes {
			if !now.After(spawnTime) {
				continue
			}

			// Figure out which category to launch
			category, rateSum := sampleRateMap(s.DepartureRates[airport][runway])
			if rateSum == 0 {
				lg.Errorf("%s/%s: couldn't find a matching runway for spawning departure?", airport, runway)
				continue
			}

			ap := sim.GetAirport(airport)
			idx := FindIf(sim.DepartureRunways,
				func(r ScenarioGroupDepartureRunway) bool {
					return r.Airport == airport && r.Runway == runway && r.Category == category
				})
			if idx == -1 {
				lg.Errorf("%s/%s/%s: couldn't find airport/runway/category for spawning departure. rates %s dep runways %s",
					airport, runway, category, spew.Sdump(s.DepartureRates[airport][runway]), spew.Sdump(sim.DepartureRunways))
				continue
			}

			if ac := s.SpawnDeparture(ap, &sim.DepartureRunways[idx]); ac != nil {
				ac.FlightPlan.DepartureAirport = airport
				addAircraft(ac)
				s.NextDepartureSpawn[airport][runway] = now.Add(randomWait(rateSum))
			}
		}
	}
}

var badCallsigns map[string]interface{} = map[string]interface{}{
	// 9/11
	"AAL11":  nil,
	"UAL175": nil,
	"AAL77":  nil,
	"UAL93":  nil,

	// Pilot suicide
	"MAS17":   nil,
	"MAS370":  nil,
	"GWI18G":  nil,
	"GWI9525": nil,
	"MSR990":  nil,

	// Hijackings
	"FDX705":  nil,
	"AFR8969": nil,

	// Selected major crashes (leaning toward callsigns vice uses or is
	// likely to use in the future, via
	// https://en.wikipedia.org/wiki/List_of_deadliest_aircraft_accidents_and_incidents
	"PAA1736": nil,
	"KLM4805": nil,
	"JAL123":  nil,
	"AIC182":  nil,
	"AAL191":  nil,
	"PAA103":  nil,
	"KAL007":  nil,
	"AAL587":  nil,
	"CAL140":  nil,
	"TWA800":  nil,
	"SWR111":  nil,
	"KAL801":  nil,
	"AFR447":  nil,
	"CAL611":  nil,
	"LOT5055": nil,
	"ICE001":  nil,
}

func sampleAircraft(icao, fleet string) *Aircraft {
	al, ok := database.Airlines[icao]
	if !ok {
		// TODO: this should be caught at load validation time...
		lg.Errorf("Chose airline %s, not found in database", icao)
		return nil
	}

	if fleet == "" {
		fleet = "default"
	}

	fl, ok := al.Fleets[fleet]
	if !ok {
		// TODO: this also should be caught at validation time...
		lg.Errorf("Airline %s doesn't have a \"%s\" fleet!", icao, fleet)
		return nil
	}

	// Sample according to fleet count
	var aircraft string
	acCount := 0
	for _, ac := range fl {
		// Reservoir sampling...
		acCount += ac.Count
		if rand.Float32() < float32(ac.Count)/float32(acCount) {
			aircraft = ac.ICAO
		}
	}

	perf, ok := database.AircraftPerformance[aircraft]
	if !ok {
		// TODO: validation stage...
		lg.Errorf("Aircraft %s not found in performance database from fleet %+v, airline %s",
			aircraft, fleet, icao)
		return nil
	}

	// random callsign
	callsign := strings.ToUpper(icao)
	for {
		format := "####"
		if len(al.Callsign.CallsignFormats) > 0 {
			format = Sample(al.Callsign.CallsignFormats)
		}
		for {
			id := ""
			for _, ch := range format {
				switch ch {
				case '#':
					id += fmt.Sprintf("%d", rand.Intn(10))
				case '@':
					id += string(rune('A' + rand.Intn(26)))
				}
			}
			if id != "0" {
				callsign += id
				break
			}
		}
		// Only break and accept the callsign if it's not a bad one..
		if _, found := badCallsigns[callsign]; !found {
			break
		}
	}

	squawk := Squawk(rand.Intn(0o7000))

	acType := aircraft
	if perf.WeightClass == "H" {
		acType = "H/" + acType
	}
	if perf.WeightClass == "J" {
		acType = "J/" + acType
	}

	return &Aircraft{
		Callsign:       callsign,
		AssignedSquawk: squawk,
		Squawk:         squawk,
		Mode:           Charlie,
		FlightPlan: &FlightPlan{
			Rules:        IFR,
			AircraftType: acType,
		},

		Performance: perf,
	}
}

func (s *Server) SpawnArrival(airportName string, arrivalGroup string) *Aircraft {
	arrivals := s.Sim.ArrivalGroups[arrivalGroup]
	// Randomly sample from the arrivals that have a route to this airport.
	idx := SampleFiltered(arrivals, func(ar Arrival) bool {
		_, ok := ar.Airlines[airportName]
		return ok
	})
	if idx == -1 {
		lg.Errorf("unable to find route in arrival group %s for airport %s?!",
			arrivalGroup, airportName)
		return nil
	}
	arr := arrivals[idx]

	airline := Sample(arr.Airlines[airportName])
	ac := sampleAircraft(airline.ICAO, airline.Fleet)
	if ac == nil {
		return nil
	}

	ac.FlightPlan.DepartureAirport = airline.Airport
	ac.FlightPlan.ArrivalAirport = airportName
	ac.TrackingController = arr.InitialController
	ac.ControllingController = arr.InitialController
	ac.FlightPlan.Altitude = int(arr.CruiseAltitude)
	if ac.FlightPlan.Altitude == 0 { // unspecified
		ac.FlightPlan.Altitude = PlausibleFinalAltitude(ac.FlightPlan)
	}
	ac.FlightPlan.Route = arr.Route

	// Start with the default waypoints for the arrival; these may be
	// updated when an 'expect' approach is given...
	ac.Waypoints = arr.Waypoints
	// Hold onto these with the Aircraft so we have them later.
	ac.ArrivalRunwayWaypoints = arr.RunwayWaypoints

	ac.Altitude = arr.InitialAltitude
	ac.IAS = min(arr.InitialSpeed, ac.Performance.Speed.Cruise)

	ac.Scratchpad = arr.Scratchpad
	if arr.ExpectApproach != "" {
		ap := sim.GetAirport(ac.FlightPlan.ArrivalAirport)
		if _, ok := ap.Approaches[arr.ExpectApproach]; ok {
			ac.ApproachId = arr.ExpectApproach
		} else {
			lg.Errorf("%s: unable to find expected %s approach", ac.Callsign, arr.ExpectApproach)
			return nil
		}
	}

	if rand.Float32() < s.GoAroundRate {
		ac.AddFutureNavCommand(&GoAround{AirportDistance: 0.1 + .6*rand.Float32()})
	}

	ac.Nav.L = &FlyRoute{}
	if arr.SpeedRestriction != 0 {
		ac.Nav.S = &MaintainSpeed{IAS: min(arr.SpeedRestriction, ac.Performance.Speed.Cruise)}
	} else {
		ac.Nav.S = &FlyRoute{}
	}
	if arr.ClearedAltitude != 0 {
		ac.Nav.V = &MaintainAltitude{Altitude: arr.ClearedAltitude}
	} else {
		ac.Nav.V = &FlyRoute{}
	}

	return ac
}

func (s *Server) SpawnDeparture(ap *Airport, rwy *ScenarioGroupDepartureRunway) *Aircraft {
	var dep *Departure
	if rand.Float32() < s.DepartureChallenge {
		// 50/50 split between the exact same departure and a departure to
		// the same gate as the last departure.
		if rand.Float32() < .5 {
			dep = rwy.lastDeparture
		} else if rwy.lastDeparture != nil {
			idx := SampleFiltered(ap.Departures,
				func(d Departure) bool {
					return ap.ExitCategories[d.Exit] == ap.ExitCategories[rwy.lastDeparture.Exit]
				})
			if idx == -1 {
				// This shouldn't ever happen...
				lg.Errorf("%s: unable to find a valid departure: %s", rwy.Runway, spew.Sdump(ap))
				return nil
			}
			dep = &ap.Departures[idx]
		}
	}

	if dep == nil {
		// Sample uniformly, minding the category, if specified
		idx := SampleFiltered(ap.Departures,
			func(d Departure) bool {
				return rwy.Category == "" || rwy.Category == ap.ExitCategories[d.Exit]
			})
		if idx == -1 {
			// This shouldn't ever happen...
			lg.Errorf("%s: unable to find a valid departure: %s", rwy.Runway, spew.Sdump(ap))
			return nil
		}
		dep = &ap.Departures[idx]
	}

	rwy.lastDeparture = dep

	airline := Sample(dep.Airlines)
	ac := sampleAircraft(airline.ICAO, airline.Fleet)

	exitRoute := rwy.ExitRoutes[dep.Exit]
	ac.Waypoints = DuplicateSlice(exitRoute.Waypoints)
	ac.Waypoints = append(ac.Waypoints, dep.routeWaypoints...)

	ac.FlightPlan.Route = exitRoute.InitialRoute + " " + dep.Route
	ac.FlightPlan.ArrivalAirport = dep.Destination
	ac.Scratchpad = sim.Scratchpads[dep.Exit]
	if dep.Altitude == 0 {
		ac.FlightPlan.Altitude = PlausibleFinalAltitude(ac.FlightPlan)
	} else {
		ac.FlightPlan.Altitude = dep.Altitude
	}

	ac.TrackingController = ap.DepartureController
	ac.ControllingController = ap.DepartureController
	ac.Altitude = float32(ap.Elevation)
	ac.IsDeparture = true

	ac.Nav.L = &FlyRoute{}
	ac.Nav.S = &FlyRoute{}
	ac.Nav.V = &MaintainAltitude{Altitude: float32(ap.Elevation)}

	ac.AddFutureNavCommand(&ClimbOnceAirborne{
		Altitude: float32(min(exitRoute.ClearedAltitude, ac.FlightPlan.Altitude)),
	})

	return ac
}

///////////////////////////////////////////////////////////////////////////
// Commands from the user

type AircraftSpecifier struct {
	ControllerToken string
	Callsign        string
}

type AircraftPropertiesSpecifier struct {
	ControllerToken string
	Callsign        string
	Scratchpad      string
}

func (s *Server) dispatchCommand(token string, callsign string,
	check func(c *Controller, ac *Aircraft) error,
	cmd func(*Controller, *Aircraft) (string, error), response *string) error {
	if sc, ok := s.controllers[token]; !ok {
		return ErrInvalidControllerToken
	} else if ac, ok := s.Sim.Aircraft[callsign]; !ok {
		return ErrNoAircraftForCallsign
	} else {
		ctrl := sim.GetController(sc.Callsign)
		if ctrl == nil {
			panic("wtf")
		}

		if err := check(ctrl, ac); err != nil {
			return err
		} else {
			resp, err := cmd(ctrl, ac)
			if response != nil {
				*response = resp
			}
			return err
		}
	}
}

// Commands that are allowed by the controlling controller, who may not still have the track;
// e.g., turns after handoffs.
func (s *Server) dispatchControllingCommand(token string, callsign string,
	cmd func(*Controller, *Aircraft) (string, error), response *string) error {
	return s.dispatchCommand(token, callsign,
		func(ctrl *Controller, ac *Aircraft) error {
			if ac.ControllingController != ctrl.Callsign {
				return ErrOtherControllerHasTrack
			}
			return nil
		},
		cmd, response)
}

// Commands that are allowed by tracking controller only.
func (s *Server) dispatchTrackingCommand(token string, callsign string,
	cmd func(*Controller, *Aircraft) (string, error), response *string) error {
	return s.dispatchCommand(token, callsign,
		func(ctrl *Controller, ac *Aircraft) error {
			if ac.TrackingController != ctrl.Callsign {
				return ErrOtherControllerHasTrack
			}
			return nil
		},
		cmd, response)
}

func (s *Server) SetScratchpad(a *AircraftPropertiesSpecifier, _ *struct{}) error {
	return s.dispatchTrackingCommand(a.ControllerToken, a.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			ac.Scratchpad = a.Scratchpad
			eventStream.Post(&ModifiedAircraftEvent{ac: ac})
			return "", nil
		}, nil)
}

func (s *Server) InitiateTrack(a *AircraftSpecifier, _ *struct{}) error {
	return s.dispatchCommand(a.ControllerToken, a.Callsign,
		func(c *Controller, ac *Aircraft) error {
			// Make sure no one has the track already
			if ac.TrackingController != "" {
				return ErrOtherControllerHasTrack
			}
			return nil
		},
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			ac.TrackingController = ctrl.Callsign
			ac.ControllingController = ctrl.Callsign
			eventStream.Post(&ModifiedAircraftEvent{ac: ac})
			eventStream.Post(&InitiatedTrackEvent{ac: ac})
			return "", nil
		}, nil)
}

func (s *Server) DropTrack(a *AircraftSpecifier, _ *struct{}) error {
	return s.dispatchTrackingCommand(a.ControllerToken, a.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			ac.TrackingController = ""
			ac.ControllingController = ""
			eventStream.Post(&ModifiedAircraftEvent{ac: ac})
			eventStream.Post(&DroppedTrackEvent{ac: ac})
			return "", nil
		}, nil)
}

type HandoffSpecifier struct {
	ControllerToken string
	Callsign        string
	Controller      string
}

func (s *Server) Handoff(h *HandoffSpecifier, _ *struct{}) error {
	return s.dispatchCommand(h.ControllerToken, h.Callsign,
		func(ctrl *Controller, ac *Aircraft) error {
			if ac.TrackingController != ctrl.Callsign {
				return ErrOtherControllerHasTrack
			}
			return nil
		},
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			if octrl := sim.GetController(h.Controller); octrl == nil {
				return "", ErrNoController
			} else {
				ac.OutboundHandoffController = octrl.Callsign
				eventStream.Post(&ModifiedAircraftEvent{ac: ac})
				acceptDelay := 4 + rand.Intn(10)
				s.Handoffs[ac.Callsign] = sim.CurrentTime().Add(time.Duration(acceptDelay) * time.Second)
				return "", nil
			}
		}, nil)
}

func (s *Server) AcceptHandoff(a *AircraftSpecifier, _ *struct{}) error {
	return s.dispatchCommand(a.ControllerToken, a.Callsign,
		func(ctrl *Controller, ac *Aircraft) error {
			if ac.InboundHandoffController != ctrl.Callsign {
				return ErrNotBeingHandedOffToMe
			}
			return nil
		},
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			ac.InboundHandoffController = ""
			ac.TrackingController = ctrl.Callsign
			ac.ControllingController = ctrl.Callsign
			eventStream.Post(&AcceptedHandoffEvent{controller: ctrl.Callsign, ac: ac})
			eventStream.Post(&ModifiedAircraftEvent{ac: ac}) // FIXME...
			return "", nil
		}, nil)
}

func (s *Server) CancelHandoff(a *AircraftSpecifier, _ *struct{}) error {
	return s.dispatchTrackingCommand(a.ControllerToken, a.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			delete(s.Handoffs, ac.Callsign)

			ac.OutboundHandoffController = ""
			// TODO: we are inconsistent in other control backends about events
			// when user does things like this; sometimes no event, sometimes
			// modified a/c event...
			eventStream.Post(&ModifiedAircraftEvent{ac: ac})
			return "", nil
		}, nil)
}

type AltitudeAssignment struct {
	ControllerToken string
	Callsign        string
	Altitude        int
}

func (s *Server) AssignAltitude(alt *AltitudeAssignment, response *string) error {
	return s.dispatchControllingCommand(alt.ControllerToken, alt.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) { return ac.AssignAltitude(alt.Altitude) },
		response)
}

func (s *Server) SetTemporaryAltitude(alt *AltitudeAssignment, response *string) error {
	return s.dispatchTrackingCommand(alt.ControllerToken, alt.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			ac.TempAltitude = alt.Altitude
			return "", nil
		}, response)
}

type HeadingAssignment struct {
	ControllerToken string
	Callsign        string
	Heading         int
	Present         bool
	LeftDegrees     int
	RightDegrees    int
	Turn            TurnMethod
}

func (s *Server) AssignHeading(hdg *HeadingAssignment, response *string) error {
	return s.dispatchControllingCommand(hdg.ControllerToken, hdg.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			if hdg.Present {
				if _, err := ac.AssignHeading(int(ac.Heading), TurnClosest); err == nil {
					return "fly present heading", nil
				} else {
					return "", err
				}
			} else if hdg.LeftDegrees != 0 {
				return ac.TurnLeft(hdg.LeftDegrees)
			} else if hdg.RightDegrees != 0 {
				return ac.TurnRight(hdg.RightDegrees)
			} else {
				return ac.AssignHeading(hdg.Heading, hdg.Turn)
			}
		}, response)
}

type SpeedAssignment struct {
	ControllerToken string
	Callsign        string
	Speed           int
}

func (s *Server) AssignSpeed(sa *SpeedAssignment, response *string) error {
	return s.dispatchControllingCommand(sa.ControllerToken, sa.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) { return ac.AssignSpeed(sa.Speed) },
		response)
}

type FixSpecifier struct {
	ControllerToken string
	Callsign        string
	Fix             string
	Heading         int
	Altitude        int
	Speed           int
}

func (s *Server) DirectFix(f *FixSpecifier, response *string) error {
	return s.dispatchControllingCommand(f.ControllerToken, f.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) { return ac.DirectFix(f.Fix) },
		response)
}

func (s *Server) DepartFixHeading(f *FixSpecifier, response *string) error {
	return s.dispatchControllingCommand(f.ControllerToken, f.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) { return ac.DepartFixHeading(f.Fix, f.Heading) },
		response)
}

func (s *Server) CrossFixAt(f *FixSpecifier, response *string) error {
	return s.dispatchControllingCommand(f.ControllerToken, f.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) { return ac.CrossFixAt(f.Fix, f.Altitude, f.Speed) },
		response)
}

type ApproachAssignment struct {
	ControllerToken string
	Callsign        string
	Approach        string
}

func (s *Server) ExpectApproach(a *ApproachAssignment, response *string) error {
	return s.dispatchControllingCommand(a.ControllerToken, a.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) { return ac.ExpectApproach(a.Approach) },
		response)
}

type ApproachClearance struct {
	ControllerToken string
	Callsign        string
	Approach        string
	StraightIn      bool
}

func (s *Server) ClearedApproach(c *ApproachClearance, response *string) error {
	return s.dispatchControllingCommand(c.ControllerToken, c.Callsign,
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			if c.StraightIn {
				return ac.ClearedStraightInApproach(c.Approach)
			} else {
				return ac.ClearedApproach(c.Approach)
			}
		}, response)
}

func (s *Server) DeleteAircraft(a *AircraftSpecifier, _ *struct{}) error {
	return s.dispatchCommand(a.ControllerToken, a.Callsign,
		func(ctrl *Controller, ac *Aircraft) error { return nil },
		func(ctrl *Controller, ac *Aircraft) (string, error) {
			eventStream.Post(&RemovedAircraftEvent{ac: ac})
			delete(s.Sim.Aircraft, ac.Callsign)
			return "", nil
		}, nil)
}

type ServerUpdates struct {
	// events
	Events   []interface{} // GACK: no go for gob encoding...
	Aircraft map[string]*Aircraft
}

func (s *Server) GetUpdates(token string, u *ServerUpdates) error {
	return nil
}
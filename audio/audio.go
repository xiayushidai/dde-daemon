/*
 * Copyright (C) 2014 ~ 2018 Deepin Technology Co., Ltd.
 *
 * Author:     jouyouyun <jouyouwen717@gmail.com>
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package audio

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"pkg.deepin.io/lib/dbusutil/gsprop"
	"strings"
	"sync"
	"time"

	"golang.org/x/xerrors"
	"pkg.deepin.io/dde/daemon/common/dsync"
	"pkg.deepin.io/gir/gio-2.0"
	dbus "pkg.deepin.io/lib/dbus1"
	"pkg.deepin.io/lib/dbusutil"
	"pkg.deepin.io/lib/gsettings"
	"pkg.deepin.io/lib/pulse"
)

const (
	gsSchemaAudio                 = "com.deepin.dde.audio"
	gsKeyFirstRun                 = "first-run"
	gsKeyInputVolume              = "input-volume"
	gsKeyOutputVolume             = "output-volume"
	gsKeyHeadphoneOutputVolume    = "headphone-output-volume"
	gsKeyHeadphoneUnplugAutoPause = "headphone-unplug-auto-pause"
	gsKeyVolumeIncrease           = "volume-increase"

	gsSchemaSoundEffect = "com.deepin.dde.sound-effect"
	gsKeyEnabled        = "enabled"

	dbusServiceName = "com.deepin.daemon.Audio"
	dbusPath        = "/com/deepin/daemon/Audio"
	dbusInterface   = dbusServiceName

	cmdSystemctl  = "systemctl"
	cmdPulseaudio = "pulseaudio"
	
	increaseMaxVolume = 1.5
	normalMaxVolume	  = 1.0
)

var (
	defaultInputVolume           = 0.1
	defaultOutputVolume          = 0.5
	defaultHeadphoneOutputVolume = 0.17
	gMaxUIVolume float64
)

//go:generate dbusutil-gen -type Audio,Sink,SinkInput,Source,Meter -import pkg.deepin.io/lib/dbus1 audio.go sink.go sinkinput.go source.go meter.go

func objectPathSliceEqual(v1, v2 []dbus.ObjectPath) bool {
	if len(v1) != len(v2) {
		return false
	}
	for i, e1 := range v1 {
		if e1 != v2[i] {
			return false
		}
	}
	return true
}

type Audio struct {
	service *dbusutil.Service
	PropsMu sync.RWMutex
	// dbusutil-gen: equal=objectPathSliceEqual
	SinkInputs []dbus.ObjectPath
	// dbusutil-gen: equal=objectPathSliceEqual
	Sinks []dbus.ObjectPath
	// dbusutil-gen: equal=objectPathSliceEqual
	Sources        []dbus.ObjectPath
	DefaultSink    dbus.ObjectPath
	DefaultSource  dbus.ObjectPath
	Cards          string
	IncreaseVolume gsprop.Bool `prop:"access:rw"`
	defaultPaCfg   defaultPaConfig

	// dbusutil-gen: ignore
	// 最大音量
	MaxUIVolume float64 // readonly

	headphoneUnplugAutoPause bool

	inited    bool
	settings  *gio.Settings
	ctx       *pulse.Context
	eventChan chan *pulse.Event
	stateChan chan int

	// 正常输出声音的程序列表
	sinkInputs        map[uint32]*SinkInput
	defaultSink       *Sink
	defaultSource     *Source
	sinks             map[uint32]*Sink
	sources           map[uint32]*Source
	defaultSinkName   string
	defaultSourceName string
	meters            map[string]*Meter
	mu                sync.Mutex
	quit              chan struct{}

	cards CardList

	isSaving    bool
	saverLocker sync.Mutex

	portLocker sync.Mutex

	syncConfig     *dsync.Config
	sessionSigLoop *dbusutil.SignalLoop

	noRestartPulseAudio bool

	methods *struct {
		SetPort func() `in:"cardId,portName,direction"`
	}
}

func newAudio(service *dbusutil.Service) *Audio {
	a := &Audio{
		service:     service,
		meters:      make(map[string]*Meter),
		MaxUIVolume: pulse.VolumeUIMax,
	}

	a.settings = gio.NewSettings(gsSchemaAudio)
	a.settings.Reset(gsKeyInputVolume)
	a.IncreaseVolume.Bind(a.settings, gsKeyVolumeIncrease)
	a.headphoneUnplugAutoPause = a.settings.GetBoolean(gsKeyHeadphoneUnplugAutoPause)
	if a.IncreaseVolume.Get(){
		a.MaxUIVolume = increaseMaxVolume
	} else {
		a.MaxUIVolume = normalMaxVolume
	}
	gMaxUIVolume = a.MaxUIVolume
	gsettings.ConnectChanged(gsSchemaAudio, gsKeyVolumeIncrease, func(val string) {
		tm := a.settings.GetBoolean(gsKeyVolumeIncrease)
		if tm {
			a.MaxUIVolume = increaseMaxVolume
		} else {
			a.MaxUIVolume = normalMaxVolume
		}
		gMaxUIVolume = a.MaxUIVolume
		a.service.EmitPropertyChanged(a, "MaxUIVolume", a.MaxUIVolume)
	})

	a.sessionSigLoop = dbusutil.NewSignalLoop(service.Conn(), 10)
	a.syncConfig = dsync.NewConfig("audio", &syncConfig{a: a},
		a.sessionSigLoop, dbusPath, logger)
	a.sessionSigLoop.Start()
	return a
}

func startPulseaudio() error {
	var errBuf bytes.Buffer
	cmd := exec.Command(cmdSystemctl, "--user", "start", "pulseaudio")
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		logger.Warningf("failed to start pulseaudio via systemd: err: %v, stderr: %s",
			err, errBuf.Bytes())
	}
	errBuf.Reset()

	err = exec.Command(cmdPulseaudio, "--check").Run()
	if err == nil {
		return nil
	}

	cmd = exec.Command(cmdPulseaudio, "--start") //如果pulseaudio没有启动，则启动
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if err != nil {
		logger.Warningf("failed to start pulseaudio via `pulseaudio --start`: err: %v, stderr: %s",
			err, errBuf.Bytes())
		return xerrors.Errorf("cmd `pulseaudio --start` error:", err)
	}
	return nil
}

func getCtx() (ctx *pulse.Context, err error) {
	err = startPulseaudio()
	if err != nil {
		err = xerrors.Errorf("failed to start pulseaudio: %w", err)
		return
	}

	ctx = pulse.GetContextForced()
	if ctx == nil {
		err = errors.New("failed to get pulse context")
		return
	}
	ctx.LoadModule("module-switch-on-connect", "")  //加载一个模块
	return
}

func (a *Audio) init() error {
	a.initDefaultVolumes()
	ctx, err := getCtx()
	if err != nil {
		return xerrors.Errorf("failed to get context: %w", err)
	}

	a.defaultPaCfg = loadDefaultPaConfig(defaultPaFile)
	logger.Debugf("defaultPaConfig: %+v", a.defaultPaCfg)
	a.mu.Lock()
	a.ctx = ctx

	// init a.sinks
	a.sinks = make(map[uint32]*Sink)
	sinkInfoList := a.ctx.GetSinkList()
	for _, sinkInfo := range sinkInfoList {
		sink := newSink(sinkInfo, a)
		a.sinks[sinkInfo.Index] = sink
		sinkPath := sink.getPath()
		err := a.service.Export(sinkPath, sink)
		if err != nil {
			logger.Warning(err)
		}
	}

	// init a.sources
	a.sources = make(map[uint32]*Source)
	sourceInfoList := a.ctx.GetSourceList()
	for _, sourceInfo := range sourceInfoList {
		source := newSource(sourceInfo, a)
		a.sources[sourceInfo.Index] = source
		sourcePath := source.getPath()
		err := a.service.Export(sourcePath, source)
		if err != nil {
			logger.Warning(err)
		}
	}

	// init a.sinkInputs
	a.sinkInputs = make(map[uint32]*SinkInput)
	sinkInputInfoList := a.ctx.GetSinkInputList()
	for _, sinkInputInfo := range sinkInputInfoList {
		sinkInput := newSinkInput(sinkInputInfo, a)
		a.sinkInputs[sinkInputInfo.Index] = sinkInput
		if sinkInput.visible {
			err := a.service.Export(sinkInput.getPath(), sinkInput)
			if err != nil {
				logger.Warning(err)
			}
		}
	}
	a.mu.Unlock()
	a.updatePropSinks()
	a.updatePropSources()
	a.updatePropSinkInputs()

	serverInfo, err := a.ctx.GetServer()
	if err == nil {
		a.mu.Lock()
		a.defaultSourceName = serverInfo.DefaultSourceName
		a.defaultSinkName = serverInfo.DefaultSinkName

		for _, sink := range a.sinks {
			if sink.Name == a.defaultSinkName {
				a.defaultSink = sink
				a.PropsMu.Lock()
				a.setPropDefaultSink(sink.getPath())
				a.PropsMu.Unlock()
			}
		}

		for _, source := range a.sources {
			if source.Name == a.defaultSourceName {
				a.defaultSource = source
				a.PropsMu.Lock()
				a.setPropDefaultSource(source.getPath())
				a.PropsMu.Unlock()
			}
		}
		a.mu.Unlock()
	} else {
		logger.Warning(err)
	}

	a.mu.Lock()
	a.cards = newCardList(a.ctx.GetCardList())

	a.PropsMu.Lock()
	a.setPropCards(a.cards.string())
	a.PropsMu.Unlock()

	a.eventChan = make(chan *pulse.Event, 100)
	a.stateChan = make(chan int, 10)
	a.quit = make(chan struct{})
	a.ctx.AddEventChan(a.eventChan)
	a.ctx.AddStateChan(a.stateChan)

	a.mu.Unlock()

	go a.handleEvent()
	go a.handleStateChanged()
	logger.Debug("init done")

	firstRun := a.settings.GetBoolean(gsKeyFirstRun)
	logger.Warning("firstrun is ",firstRun)
	if firstRun {
		logger.Info("first run, Will remove old audio config")
		err := removeConfig()
		if err != nil && !os.IsNotExist(err) {
			logger.Warning("Failed to remove audio config:", err)
		}
		a.resetSinksVolume()
		a.resetSourceVolume()
		a.trySelectBestPort()
		a.settings.SetBoolean(gsKeyFirstRun, false)
	} else {//当dde-session-daemon启动时，从audio.json读取配置文件，然后通过pa设置音量。
		a.applyConfig()
	}

	a.fixActivePortNotAvailable()
	a.moveSinkInputsToDefaultSink()
	return nil
}

func (a *Audio) destroyCtxRelated() {
	a.mu.Lock()
	a.ctx.RemoveEventChan(a.eventChan)
	a.ctx.RemoveStateChan(a.stateChan)
	close(a.quit)
	a.ctx = nil

	for _, sink := range a.sinks {
		err := a.service.StopExportByPath(sink.getPath())
		if err != nil {
			logger.Warningf("failed to stop export sink #%d: %v", sink.index, err)
		}
	}
	a.sinks = nil

	for _, source := range a.sources {
		err := a.service.StopExportByPath(source.getPath())
		if err != nil {
			logger.Warningf("failed to stop export source #%d: %v", source.index, err)
		}
	}
	a.sources = nil

	for _, sinkInput := range a.sinkInputs {
		err := a.service.StopExportByPath(sinkInput.getPath())
		if err != nil {
			logger.Warningf("failed to stop export sink input #%d: %v", sinkInput.index, err)
		}
	}
	a.sinkInputs = nil

	for _, meter := range a.meters {
		err := a.service.StopExport(meter)
		if err != nil {
			logger.Warning(err)
		}
	}
	a.mu.Unlock()
}

func (a *Audio) destroy() {
	a.settings.Unref()
	a.sessionSigLoop.Stop()
	a.syncConfig.Destroy()
	a.destroyCtxRelated()
}

func (a *Audio) initDefaultVolumes() {
	inVolumePer := float64(a.settings.GetInt(gsKeyInputVolume)) / 100.0
	outVolumePer := float64(a.settings.GetInt(gsKeyOutputVolume)) / 100.0
	headphoneOutVolumePer := float64(a.settings.GetInt(gsKeyHeadphoneOutputVolume)) / 100.0
	defaultInputVolume = inVolumePer
	defaultOutputVolume = outVolumePer
	defaultHeadphoneOutputVolume = headphoneOutVolumePer
}

func (a *Audio) findSinkByCardIndexPortName(cardId uint32, portName string) *pulse.Sink {
	for _, sink := range a.ctx.GetSinkList() {
		if isPortExists(portName, sink.Ports) && sink.Card == cardId {
			return sink
		}
	}
	return nil
}

func (a *Audio) findSourceByCardIndexPortName(cardId uint32, portName string) *pulse.Source {
	for _, source := range a.ctx.GetSourceList() {
		if isPortExists(portName, source.Ports) && source.Card == cardId {
			return source
		}
	}
	return nil
}

// set default sink and sink active port
func (a *Audio) setDefaultSinkWithPort(cardId uint32, portName string) error {
	logger.Debugf("setDefaultSinkWithPort card #%d port %q", cardId, portName)
	sink := a.findSinkByCardIndexPortName(cardId, portName)
	if sink == nil {
		return fmt.Errorf("cannot find valid sink for card #%d and port %q",
			cardId, portName)
	}
	if sink.ActivePort.Name != portName {
		logger.Debugf("set sink #%d port %s", sink.Index, portName)
		a.ctx.SetSinkPortByIndex(sink.Index, portName)
	}
	if a.getDefaultSinkName() != sink.Name {
		logger.Debugf("set default sink #%d %s", sink.Index, sink.Name)
		a.ctx.SetDefaultSink(sink.Name)
	}
	return nil
}

func (a *Audio) getDefaultSinkActivePortName() string {
	defaultSink := a.getDefaultSink()
	if defaultSink == nil {
		return ""
	}

	defaultSink.PropsMu.RLock()
	name := defaultSink.ActivePort.Name
	defaultSink.PropsMu.RUnlock()
	return name
}

func (a *Audio) getDefaultSourceActivePortName() string {
	defaultSource := a.getDefaultSource()
	if defaultSource == nil {
		return ""
	}

	defaultSource.PropsMu.RLock()
	name := defaultSource.ActivePort.Name
	defaultSource.PropsMu.RUnlock()
	return name
}

// set default source and source active port
func (a *Audio) setDefaultSourceWithPort(cardId uint32, portName string) error {
	logger.Debugf("setDefault card #%d port %q", cardId, portName)
	source := a.findSourceByCardIndexPortName(cardId, portName)
	if source == nil {
		return fmt.Errorf("cannot find valid source for card #%d and port %q",
			cardId, portName)
	}

	if source.ActivePort.Name != portName {
		logger.Debugf("set source #%d port %s", source.Index, portName)
		a.ctx.SetSourcePortByIndex(source.Index, portName)
	}

	if a.getDefaultSourceName() != source.Name {
		logger.Debugf("set default source #%d %s", source.Index, source.Name)
		a.ctx.SetDefaultSource(source.Name)
	}

	return nil
}

// SetPort activate the port for the special card.
// The available sinks and sources will also change with the profile changing.
func (a *Audio) SetPort(cardId uint32, portName string, direction int32) *dbus.Error {
	logger.Debugf("Audio.SetPort card idx: %d, port name: %q, direction: %d",
		cardId, portName, direction)
	err := a.setPort(cardId, portName, int(direction))
	return dbusutil.ToError(err)
}

func (a *Audio) setPort(cardId uint32, portName string, direction int) error {
	a.portLocker.Lock()
	defer a.portLocker.Unlock()
	var (
		oppositePort      string
		oppositeDirection int
	)
	switch direction {
	case pulse.DirectionSink:
		oppositePort = a.getDefaultSourceActivePortName()
		oppositeDirection = pulse.DirectionSource
	case pulse.DirectionSource:
		oppositePort = a.getDefaultSinkActivePortName()
		oppositeDirection = pulse.DirectionSink
	default:
		return fmt.Errorf("invalid port direction: %d", direction)
	}

	a.mu.Lock()
	card, _ := a.cards.get(cardId)
	a.mu.Unlock()
	if card == nil {
		return fmt.Errorf("not found card #%d", cardId)
	}

	var err error
	targetPortInfo, err := card.Ports.Get(portName, direction)
	if err != nil {
		return err
	}

	setDefaultPort := func() error {
		if int(direction) == pulse.DirectionSink {
			return a.setDefaultSinkWithPort(cardId, portName)  //真正的设置活动端口。
		}
		return a.setDefaultSourceWithPort(cardId, portName)
	}

	if targetPortInfo.Profiles.Exists(card.ActiveProfile.Name) {
		// no need to change profile
		return setDefaultPort()
	}

	// match the common profile contain sinkPort and sourcePort
	oppositePortInfo, _ := card.Ports.Get(oppositePort, oppositeDirection)
	commonProfiles := getCommonProfiles(targetPortInfo, oppositePortInfo)
	var targetProfile string
	if len(commonProfiles) != 0 {
		targetProfile = commonProfiles[0].Name
	} else {
		name, err := card.tryGetProfileByPort(portName)
		if err != nil {
			return err
		}
		targetProfile = name
	}
	// workaround for bluetooth, set profile to 'a2dp_sink' when port direction is output
	if direction == pulse.DirectionSink && targetPortInfo.Profiles.Exists("a2dp_sink") {
		targetProfile = "a2dp_sink"
	}
	card.core.SetProfile(targetProfile)
	logger.Debug("set profile", targetProfile)
	return setDefaultPort()
}

func (a *Audio) resetSinksVolume() {
	logger.Debug("reset sink volume")
	for _, s := range a.ctx.GetSinkList() {
		a.ctx.SetSinkMuteByIndex(s.Index, false)
		curPort := s.ActivePort.Name
		portList := s.Ports
		sidx := s.Index
		for _, port := range portList {
			a.ctx.SetSinkPortByIndex(sidx, port.Name)
			// wait port active
			time.Sleep(time.Millisecond * 100)
			s, _ = a.ctx.GetSink(sidx)
			pname := strings.ToLower(port.Name)
			var cv pulse.CVolume
			if strings.Contains(pname, "headphone") || strings.Contains(pname, "headset") {
				cv = s.Volume.SetAvg(defaultHeadphoneOutputVolume).SetBalance(s.ChannelMap,
					0).SetFade(s.ChannelMap, 0)
			} else {
				cv = s.Volume.SetAvg(defaultOutputVolume).SetBalance(s.ChannelMap,
					0).SetFade(s.ChannelMap, 0)
			}
			a.ctx.SetSinkVolumeByIndex(sidx, cv)
			time.Sleep(time.Millisecond * 100)
		}
		a.ctx.SetSinkPortByIndex(sidx, curPort)
	}
}

func (a *Audio) resetSourceVolume() {
	logger.Debug("reset source volume")
	for _, s := range a.ctx.GetSourceList() {
		a.ctx.SetSourceMuteByIndex(s.Index, false)
		cv := s.Volume.SetAvg(defaultInputVolume).SetBalance(s.ChannelMap,
			0).SetFade(s.ChannelMap, 0)
		logger.Debug("defaultInputVolume = ",defaultInputVolume,cv)
		a.ctx.SetSourceVolumeByIndex(s.Index, cv)
	}
}

func (a *Audio) Reset() *dbus.Error {
	a.resetSinksVolume()
	a.resetSourceVolume()
	gsSoundEffect := gio.NewSettings(gsSchemaSoundEffect)
	gsSoundEffect.Reset(gsKeyEnabled)
	gsSoundEffect.Unref()
	return nil
}

func (a *Audio) moveSinkInputsToSink(sinkId uint32) {
	a.mu.Lock()
	if len(a.sinkInputs) == 0 {
		a.mu.Unlock()
		return
	}
	var list []uint32
	for _, sinkInput := range a.sinkInputs {
		if sinkInput.getPropSinkIndex() == sinkId {
			continue
		}

		list = append(list, sinkInput.index)
	}
	a.mu.Unlock()
	if len(list) == 0 {
		return
	}
	logger.Debugf("move sink inputs %v to sink #%d", list, sinkId)
	a.ctx.MoveSinkInputsByIndex(list, sinkId)
}

func isPortExists(name string, ports []pulse.PortInfo) bool {
	for _, port := range ports {
		if port.Name == name {
			return true
		}
	}
	return false
}

func (*Audio) GetInterfaceName() string {
	return dbusInterface
}

func (a *Audio) updateDefaultSink(sinkName string) {
	sinkInfo := a.getSinkInfoByName(sinkName)
	if sinkInfo == nil {
		logger.Warning("failed to get sinkInfo for name:", sinkName)
		return
	}
	logger.Debugf("updateDefaultSink #%d %s", sinkInfo.Index, sinkName)
	a.moveSinkInputsToSink(sinkInfo.Index)

	a.mu.Lock()
	sink, ok := a.sinks[sinkInfo.Index]
	if !ok {
		a.mu.Unlock()
		logger.Warningf("not found sink #%d", sinkInfo.Index)
		return
	}

	a.defaultSink = sink
	defaultSinkPath := sink.getPath()  //com/deepin/daemon/sink1
	a.mu.Unlock()

	a.PropsMu.Lock()
	a.setPropDefaultSink(defaultSinkPath)
	a.PropsMu.Unlock()
	logger.Debug("set prop default sink:", defaultSinkPath)
}

func (a *Audio) updateDefaultSource(sourceName string) {
	sourceInfo := a.getSourceInfoByName(sourceName)
	if sourceInfo == nil {
		logger.Warning("failed to get sourceInfo for name:", sourceName)
		return
	}
	logger.Debugf("updateDefaultSource #%d %s", sourceInfo.Index, sourceName)

	a.mu.Lock()
	source, ok := a.sources[sourceInfo.Index]
	if !ok {
		a.mu.Unlock()
		logger.Warningf("not found source #%d", sourceInfo.Index)
		return
	}
	a.defaultSource = source
	defaultSourcePath := source.getPath()
	a.mu.Unlock()

	a.PropsMu.Lock()
	a.setPropDefaultSource(defaultSourcePath)
	a.PropsMu.Unlock()
}

func (a *Audio) context() *pulse.Context {
	a.mu.Lock()
	c := a.ctx
	a.mu.Unlock()
	return c
}

func (a *Audio) getSinkInput(index uint32) *SinkInput {
	a.mu.Lock()

	for _, sinkInput := range a.sinkInputs {
		if sinkInput.index == index {
			a.mu.Unlock()
			return sinkInput
		}
	}

	a.mu.Unlock()
	return nil
}

func (a *Audio) moveSinkInputsToDefaultSink() {
	a.mu.Lock()
	if a.defaultSink == nil {
		a.mu.Unlock()
		return
	}
	defaultSinkIndex := a.defaultSink.index
	a.mu.Unlock()
	a.moveSinkInputsToSink(defaultSinkIndex)
}

func (a *Audio) getDefaultSource() *Source {
	a.mu.Lock()
	v := a.defaultSource
	a.mu.Unlock()
	return v
}

func (a *Audio) getDefaultSourceName() string {
	source := a.getDefaultSource()
	if source == nil {
		return ""
	}

	source.PropsMu.RLock()
	v := source.Name
	source.PropsMu.RUnlock()
	return v
}

func (a *Audio) getDefaultSink() *Sink {
	a.mu.Lock()
	v := a.defaultSink
	a.mu.Unlock()
	return v
}

func (a *Audio) getDefaultSinkName() string {
	sink := a.getDefaultSink()
	if sink == nil {
		return ""
	}

	sink.PropsMu.RLock()
	v := sink.Name
	sink.PropsMu.RUnlock()
	return v
}

func (a *Audio) getSinkInfoByName(sinkName string) *pulse.Sink {
	for _, sinkInfo := range a.ctx.GetSinkList() {
		if sinkInfo.Name == sinkName {
			return sinkInfo
		}
	}
	return nil
}

func (a *Audio) getSourceInfoByName(sourceName string) *pulse.Source {
	for _, sourceInfo := range a.ctx.GetSourceList() {
		if sourceInfo.Name == sourceName {
			return sourceInfo
		}
	}
	return nil
}
func getBestPort(ports []pulse.PortInfo) pulse.PortInfo {
	var portUnknown pulse.PortInfo
	var portYes pulse.PortInfo
	for _, port := range ports {
		if port.Available == pulse.AvailableTypeYes {
			if port.Priority > portYes.Priority || portYes.Name == "" {
				portYes = port
			}
		} else if port.Available == pulse.AvailableTypeUnknow {
			if port.Priority > portUnknown.Priority || portUnknown.Name == "" {
				portUnknown = port
			}
		}
	}

	if portYes.Name != "" {
		return portYes
	}
	return portUnknown
}

func (a *Audio) fixActivePortNotAvailable() {
	sinkInfoList := a.ctx.GetSinkList()
	for _, sinkInfo := range sinkInfoList {
		activePort := sinkInfo.ActivePort

		if activePort.Available == pulse.AvailableTypeNo {
			newPort := getBestPort(sinkInfo.Ports)
			if newPort.Name != activePort.Name && newPort.Name != "" {
				logger.Info("auto switch to port", newPort.Name)
				a.ctx.SetSinkPortByIndex(sinkInfo.Index, newPort.Name)
				a.saveConfig()
			}
		}
	}
}

func (a *Audio) NoRestartPulseAudio() *dbus.Error {
	a.noRestartPulseAudio = true
	return nil
}

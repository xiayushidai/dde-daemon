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
	"pkg.deepin.io/lib/log"
	"pkg.deepin.io/lib/pulse"
)
import (
	"sort"
	"strconv"
	"time"

	"pkg.deepin.io/lib/dbus1"

)

func (a *Audio) handleEvent() { //处理声音事件
	logger:=log.NewLogger("sound")
	for {
		select {
		case event := <-a.eventChan:
			logger.Warning("this is handEvent")
			switch event.Facility {
			case pulse.FacilityServer:  //server变更是什么意思呢？   难道指dbus服务？
				a.handleServerEvent(event.Type)
				a.saveConfig()
			case pulse.FacilityCard:	//声卡变更
				a.handleCardEvent(event.Type, event.Index)
				a.saveConfig()
			case pulse.FacilitySink:  //输出设备变更。
				a.handleSinkEvent(event.Type, event.Index)
				a.saveConfig()  //将改变保存到audio.json中。
			case pulse.FacilitySource:  //输入设备变更
				a.handleSourceEvent(event.Type, event.Index)
				a.saveConfig()
			case pulse.FacilitySinkInput:
				a.handleSinkInputEvent(event.Type, event.Index)
			}

		case <-a.quit:
			logger.Debug("handleEvent return")
			return
		}
	}
}

func (a *Audio) handleStateChanged() {
	for {
		select {
		case state := <-a.stateChan:
			switch state {
			case pulse.ContextStateFailed:
				logger.Warning("pulseaudio context state failed")
				a.destroyCtxRelated()

				if !a.noRestartPulseAudio {
					logger.Debug("retry init")
					err := a.init()
					if err != nil {
						logger.Warning("failed to init:", err)
					}
					return
				} else {
					logger.Debug("do not restart pulseaudio")
				}
			}

		case <-a.quit:
			logger.Debug("handleStateChanged return")
			return
		}
	}
}

func (a *Audio) handleCardEvent(eventType int, idx uint32) {  //处理声卡变更事件。
	switch eventType {
	case pulse.EventTypeNew:
		logger.Debugf("[Event] card #%d added", idx)
		cardInfo, err := a.ctx.GetCard(idx)
		if nil != err {
			logger.Warning("get card info failed: ", err)
			return
		}
		cards, added := a.cards.add(newCard(cardInfo))
		if added {
			a.PropsMu.Lock()
			a.setPropCards(cards.string())
			a.PropsMu.Unlock()
			a.cards = cards
		}
		// fix change profile not work
		time.AfterFunc(time.Millisecond*500, func() {
			selectNewCardProfile(cardInfo)
			logger.Debug("After select profile:", cardInfo.ActiveProfile.Name)
		})
	case pulse.EventTypeRemove:
		logger.Debugf("[Event] card #%d removed", idx)
		cards, deleted := a.cards.delete(idx)
		if deleted {
			a.PropsMu.Lock()
			a.setPropCards(cards.string())
			a.PropsMu.Unlock()
			a.cards = cards
		}
	case pulse.EventTypeChange:   //声卡变更会触发该信号。
		logger.Debugf("[Event] card #%d changed", idx)
		cardInfo, err := a.ctx.GetCard(idx)  //通过idx获取cardinfo
		if nil != err {
			logger.Warning("get card info failed: ", err)
			return
		}
		a.mu.Lock()
		card, _ := a.cards.get(idx)
		if card != nil {
			card.update(cardInfo)  //更新声卡信息。
			a.PropsMu.Lock()
			a.setPropCards(a.cards.string())
			a.PropsMu.Unlock()
		}
		a.mu.Unlock()
	}
}

func (a *Audio) addSink(sinkInfo *pulse.Sink) {  //如果添加了输出设备。
	sink := newSink(sinkInfo, a)  //获取输出设备的index和props

	a.mu.Lock()
	a.sinks[sinkInfo.Index] = sink  //键值对
	a.mu.Unlock()

	sinkPath := sink.getPath()  //获取obj  path。
	err := a.service.Export(sinkPath, sink)  //将新增设备的dbus借口暴露给外面。
	if err != nil {
		logger.Warningf("failed to export sink #%d: %v", sink.index, err)
		return
	}
	a.updatePropSinks()  //更新输入设备的属性。
	logger.Warningf("sink name==%s,defaultSinkName=%s\n",sink.Name,a.defaultSinkName);
	if sink.Name == a.defaultSinkName {
		a.defaultSink = sink
		a.PropsMu.Lock()
		a.setPropDefaultSink(sinkPath)
		a.PropsMu.Unlock()
		logger.Debug("set prop default sink:", sinkPath)
	}
}

func (a *Audio) handleSinkEvent(eventType int, idx uint32) {	//处理输出设备事件。
	switch eventType {
	case pulse.EventTypeNew:
		logger.Debugf("[Event] sink #%d added", idx)
		sinkInfo, err := a.ctx.GetSink(idx)
		if err != nil {
			logger.Warning(err)
			return
		}

		a.mu.Lock()
		_, ok := a.sinks[idx]
		a.mu.Unlock()
		if ok {
			return
		}
		a.addSink(sinkInfo)

	case pulse.EventTypeRemove:
		logger.Debugf("[Event] sink #%d removed", idx)

		a.mu.Lock()
		sink, ok := a.sinks[idx]
		if !ok {
			a.mu.Unlock()
			return
		}
		delete(a.sinks, idx)
		a.mu.Unlock()
		a.updatePropSinks()

		err := a.service.StopExport(sink)
		if err != nil {
			logger.Warning(err)
		}

	case pulse.EventTypeChange:  //当输出设备改变时，出发该信号。
		logger.Debugf("[Event] sink #%d changed", idx)  //idx为新选中的是输出设备号。
		sinkInfo, err := a.ctx.GetSink(idx)  //通过idx获取输出设备信息。
		if err != nil {
			logger.Warning(err)
			return
		}

		a.mu.Lock()
		sink, ok := a.sinks[idx]  //检查该设备是否存在。
		a.mu.Unlock()
		if !ok {
			a.addSink(sinkInfo)  //不存在的话，添加输出设备，
			logger.Warning("this is return")
			return
		}
		logger.Warning("this is 218")
		sink.update(sinkInfo)	//更新
	}
}

func (a *Audio) handleSinkInputEvent(eType int, idx uint32) {
	switch eType {
	case pulse.EventTypeNew:
		logger.Debugf("[Event] sink-input #%d added", idx)
		a.handleSinkInputAdded(idx)
	case pulse.EventTypeRemove:
		logger.Debugf("[Event] sink-input #%d removed", idx)
		a.handleSinkInputRemoved(idx)
	case pulse.EventTypeChange:
		logger.Debugf("[Event] sink-input #%d changed", idx)
		sinkInputInfo, err := a.ctx.GetSinkInput(idx)
		if err != nil {
			logger.Warning(err)
			return
		}

		a.mu.Lock()
		sinkInput, ok := a.sinkInputs[idx]
		a.mu.Unlock()
		if !ok {
			return
		}
		sinkInput.update(sinkInputInfo)
	}
}

func (a *Audio) updateObjPathsProp(type0 string, ids []int, setFn func(value []dbus.ObjectPath) bool) {
	sort.Ints(ids)
	paths := make([]dbus.ObjectPath, len(ids))
	for idx, id := range ids {
		paths[idx] = dbus.ObjectPath(dbusPath + "/" + type0 + strconv.Itoa(id))
	}
	a.PropsMu.Lock()
	setFn(paths)
	a.PropsMu.Unlock()
}

func (a *Audio) updatePropSinks() {
	var ids []int
	a.mu.Lock()
	for _, sink := range a.sinks {
		ids = append(ids, int(sink.index))
	}
	a.mu.Unlock()
	a.updateObjPathsProp("Sink", ids, a.setPropSinks)  //更新obj path的属性？
}

func (a *Audio) updatePropSources() {
	var ids []int
	a.mu.Lock()
	for _, source := range a.sources {
		ids = append(ids, int(source.index))
	}
	a.mu.Unlock()
	a.updateObjPathsProp("Source", ids, a.setPropSources)
}

func (a *Audio) updatePropSinkInputs() {
	var ids []int
	a.mu.Lock()
	for _, sinkInput := range a.sinkInputs {
		if sinkInput.visible {
			ids = append(ids, int(sinkInput.index))
		}
	}
	a.mu.Unlock()
	a.updateObjPathsProp("SinkInput", ids, a.setPropSinkInputs)
}

func (a *Audio) addSinkInput(sinkInputInfo *pulse.SinkInput) {
	sinkInput := newSinkInput(sinkInputInfo, a)
	a.mu.Lock()
	a.sinkInputs[sinkInputInfo.Index] = sinkInput
	a.mu.Unlock()

	sinkInputPath := sinkInput.getPath()

	if sinkInput.visible {
		err := a.service.Export(sinkInputPath, sinkInput)
		if err != nil {
			logger.Warning(err)
			return
		}
	}
	a.updatePropSinkInputs()

	logger.Debugf("sink-input #%d play with sink #%d", sinkInputInfo.Index,
		sinkInputInfo.Sink)
}

func (a *Audio) handleSinkInputAdded(idx uint32) {
	sinkInputInfo, err := a.ctx.GetSinkInput(idx)
	if err != nil {
		logger.Warning(err)
		return
	}

	a.mu.Lock()
	_, ok := a.sinkInputs[idx]
	a.mu.Unlock()
	if ok {
		return
	}

	a.addSinkInput(sinkInputInfo)
}

func (a *Audio) handleSinkInputRemoved(idx uint32) {
	a.mu.Lock()
	sinkInput, ok := a.sinkInputs[idx]
	if !ok {
		a.mu.Unlock()
		return
	}
	delete(a.sinkInputs, idx)
	a.mu.Unlock()

	if sinkInput.visible {
		err := a.service.StopExport(sinkInput)
		if err != nil {
			logger.Warning(err)
		}
	}

	a.updatePropSinkInputs()
}

func (a *Audio) addSource(sourceInfo *pulse.Source) {
	source := newSource(sourceInfo, a)

	a.mu.Lock()
	a.sources[sourceInfo.Index] = source
	a.mu.Unlock()

	sourcePath := source.getPath()
	err := a.service.Export(sourcePath, source)
	if err != nil {
		logger.Warning(err)
		return
	}

	a.updatePropSources()

	if a.defaultSourceName == source.Name {
		a.defaultSource = source
		a.PropsMu.Lock()
		a.setPropDefaultSource(sourcePath)
		a.PropsMu.Unlock()
	}
}

func (a *Audio) handleSourceEvent(eventType int, idx uint32) {
	switch eventType {
	case pulse.EventTypeNew:
		logger.Debugf("[Event] source #%d added", idx)
		sourceInfo, err := a.ctx.GetSource(idx)
		if err != nil {
			logger.Warning(err)
			return
		}

		a.mu.Lock()
		_, ok := a.sources[idx]
		a.mu.Unlock()
		if ok {
			return
		}
		a.addSource(sourceInfo)

	case pulse.EventTypeRemove:
		logger.Debugf("[Event] source #%d removed", idx)

		a.mu.Lock()
		source, ok := a.sources[idx]
		if !ok {
			a.mu.Unlock()
			return
		}
		delete(a.sources, idx)
		a.mu.Unlock()
		a.updatePropSources()

		err := a.service.StopExport(source)
		if err != nil {
			logger.Warning(err)
			return
		}

	case pulse.EventTypeChange:
		logger.Debugf("[Event] source #%d changed", idx)
		sourceInfo, err := a.ctx.GetSource(idx)
		if err != nil {
			logger.Warning(err)
			return
		}

		a.mu.Lock()
		source, ok := a.sources[idx]
		a.mu.Unlock()
		if !ok {
			// not found source
			a.addSource(sourceInfo)
			return
		}
		source.update(sourceInfo)
	}
}

func (a *Audio) handleServerEvent(eventType int) {
	switch eventType {
	case pulse.EventTypeChange:
		server, err := a.ctx.GetServer()
		if err != nil {
			logger.Error(err)
			return
		}

		logger.Debugf("[Event] server changed: default sink: %s, default source: %s",
			server.DefaultSinkName, server.DefaultSourceName)

		a.defaultSinkName = server.DefaultSinkName
		a.defaultSourceName = server.DefaultSourceName

		a.updateDefaultSink(server.DefaultSinkName)
		a.updateDefaultSource(server.DefaultSourceName)
	}
}

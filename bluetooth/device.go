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

package bluetooth

import (
	"fmt"
	"sync"
	"time"

	bluez "github.com/linuxdeepin/go-dbus-factory/org.bluez"
	dbus "pkg.deepin.io/lib/dbus1"
	"pkg.deepin.io/lib/dbusutil"
	"pkg.deepin.io/lib/dbusutil/proxy"
)

const (
	deviceStateDisconnected  = 0
	deviceStateConnecting    = 1
	deviceStateConnected     = 2
	deviceStateDisconnecting = 3
)

type deviceState uint32

func (s deviceState) String() string {
	switch s {
	case deviceStateDisconnected:
		return "Disconnected"
	case deviceStateConnecting:
		return "doing"
	case deviceStateConnected:
		return "Connected"
    	case deviceStateDisconnecting:
       		return "Disconnecting"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

var (
	errInvalidDevicePath = fmt.Errorf("invalid device path")
)

type device struct {
	core    *bluez.Device
	adapter *adapter

	Path        dbus.ObjectPath
	AdapterPath dbus.ObjectPath

	Alias            string
	Trusted          bool
	Paired           bool
	State            deviceState
	ServicesResolved bool
	ConnectState     bool

	// optional
	UUIDs   []string
	Name    string
	Icon    string
	RSSI    int16
	Address string

	connected         bool
	connectedTime     time.Time
	retryConnectCount int
	connecting        bool
	agentWorking      bool
	isActiveDoConnect bool

	connectPhase      connectPhase
	disconnectPhase   disconnectPhase
	disconnectChan    chan struct{}
	mu                sync.Mutex
	confirmation      chan bool
	pairingFailedTime time.Time
}

func (d *device) getActiveDoConnect() bool {
	d.mu.Lock()
	value := d.isActiveDoConnect
	d.mu.Unlock()
	return value
}

func (d *device) setActiveDoConnect(value bool) {
	d.mu.Lock()
	d.isActiveDoConnect = value
	d.mu.Unlock()
}

type connectPhase uint32

const (
	connectPhaseNone = iota
	connectPhaseStart
	connectPhasePairStart
	connectPhasePairEnd
	connectPhaseConnectProfilesStart
	connectPhaseConnectProfilesEnd
)

type disconnectPhase uint32

const (
	disconnectPhaseNone = iota
	disconnectPhaseStart
	disconnectPhaseDisconnectStart
	disconnectPhaseDisconnectEnd
)

func (d *device) setDisconnectPhase(value disconnectPhase) {
	d.mu.Lock()
	d.disconnectPhase = value
	d.mu.Unlock()

	switch value {
	case disconnectPhaseDisconnectStart:
		logger.Debugf("%s disconnect start", d)
	case disconnectPhaseDisconnectEnd:
		logger.Debugf("%s disconnect end", d)
	}
	d.updateState()
	d.notifyDevicePropertiesChanged()
}

func (d *device) getDisconnectPhase() disconnectPhase {
	d.mu.Lock()
	value := d.disconnectPhase
	d.mu.Unlock()
	return value
}

func (d *device) setConnectPhase(value connectPhase) {
	d.mu.Lock()
	d.connectPhase = value
	d.mu.Unlock()

	switch value {
	case connectPhasePairStart:
		logger.Debugf("%s pair start", d)
	case connectPhasePairEnd:
		logger.Debugf("%s pair end", d)

	case connectPhaseConnectProfilesStart:
		logger.Debugf("%s connect profiles start", d)
	case connectPhaseConnectProfilesEnd:
		logger.Debugf("%s connect profiles end", d)
	}

	d.updateState()
	d.notifyDevicePropertiesChanged()
	if d.Paired && d.State == deviceStateConnected && d.ConnectState {
		notifyConnected(d.Alias)  //通知已经连接。
	}
}

func (d *device) getConnectPhase() connectPhase {
	d.mu.Lock()
	value := d.connectPhase
	d.mu.Unlock()
	return value
}

func (d *device) agentWorkStart() {
	logger.Debugf("%s agent work start", d)
	d.agentWorking = true
	d.updateState()
	d.notifyDevicePropertiesChanged()
}

func (d *device) agentWorkEnd() {
	logger.Debugf("%s agent work end", d)
	d.agentWorking = false
	d.updateState()
	d.notifyDevicePropertiesChanged()
}

func (d *device) String() string {
	return fmt.Sprintf("device [%s] %s", d.Address, d.Alias)
}

func newDevice(systemSigLoop *dbusutil.SignalLoop, dpath dbus.ObjectPath) (d *device) {
	d = &device{Path: dpath}
	systemConn := systemSigLoop.Conn()
	d.core, _ = bluez.NewDevice(systemConn, dpath)
	d.AdapterPath, _ = d.core.Adapter().Get(0)
	d.Name, _ = d.core.Name().Get(0)
	d.Alias, _ = d.core.Alias().Get(0)
	d.Address, _ = d.core.Address().Get(0)
	d.Trusted, _ = d.core.Trusted().Get(0)
	d.Paired, _ = d.core.Paired().Get(0)
	d.connected, _ = d.core.Connected().Get(0)
	d.UUIDs, _ = d.core.UUIDs().Get(0)
	d.ServicesResolved, _ = d.core.ServicesResolved().Get(0)
	d.Icon, _ = d.core.Icon().Get(0)
	d.RSSI, _ = d.core.RSSI().Get(0)
	d.updateState()
	d.disconnectChan = make(chan struct{})
	d.core.InitSignalExt(systemSigLoop, true)
	d.connectProperties()
	return
}

func (d *device) destroy() {
	d.core.RemoveHandler(proxy.RemoveAllHandlers)
}

func (d *device) notifyDeviceAdded() {  //每一个device都会通知dde-control-center，去更新状态。
	logger.Debug("DeviceAdded", d)
	err := globalBluetooth.service.Emit(globalBluetooth, "DeviceAdded", marshalJSON(d))   //bluetooth服务中的DeviceAdded信号变化,marshalJSON为传出去的参数。
	if err != nil {
		logger.Warning(err)
	}
	globalBluetooth.updateState()
}

func (d *device) notifyDevicePinCancle() {
	logger.Debug("DevicePinCancle", d)
	err := globalBluetooth.service.Emit(globalBluetooth, "PinCancle", marshalJSON(d))
	if err != nil {
		logger.Warning(err)
	}
	globalBluetooth.updateState()
}

func (d *device) notifyDeviceRemoved() {
	logger.Debug("DeviceRemoved", d)
	err := globalBluetooth.service.Emit(globalBluetooth, "DeviceRemoved", marshalJSON(d))
	if err != nil {
		logger.Warning(err)
	}
	globalBluetooth.updateState()
}

func (d *device) notifyDevicePropertiesChanged() {  //状态变更，会被dde-control捕捉。
	err := globalBluetooth.service.Emit(globalBluetooth, "DevicePropertiesChanged", marshalJSON(d))
	if err != nil {
		logger.Warning(err)
	}
	globalBluetooth.updateState()
}

func (d *device) connectProperties() {  //开始初始化的时候会使用。
	err := d.core.Connected().ConnectChanged(func(hasValue bool, connected bool) {
		if !hasValue {
			return
		}
		logger.Debugf("%s Connected: %v", d, connected)
		d.connected = connected  //连接状态

		needNotify := true  //需要通知。

		if connected { // 当前状态如果为连接。
			d.connectedTime = time.Now()
		} else {
			// when disconnected quickly after connecting, automatically try to connect
			sinceConnected := time.Since(d.connectedTime)
			logger.Debug("sinceConnected:", sinceConnected)
			logger.Debug("retryConnectCount:", d.retryConnectCount)
			d.notifyDevicePinCancle()

			if sinceConnected < 300*time.Millisecond {
				if d.retryConnectCount == 0 {
					go d.Connect()
				}
				d.retryConnectCount++
			} else if sinceConnected > 2*time.Second {
				d.retryConnectCount = 0
			}

			select {
			case d.disconnectChan <- struct{}{}:
				logger.Debugf("%s disconnectChan send done", d)
				needNotify = false
			default:
			}
		}

		d.updateState()
		d.notifyDevicePropertiesChanged()  //通知属性变更，dde-control-center会捕捉到。

		if needNotify && d.Paired && d.State == deviceStateConnected && d.ConnectState {
			d.notifyConnectedChanged()  //通知连接变化
		}
		return
	})
	if err != nil {
		logger.Warning(err)
	}

	_ = d.core.Name().ConnectChanged(func(hasValue bool, value string) {
		if !hasValue {
			return
		}
		logger.Debugf("%s Name: %v", d, value)
		d.Name = value
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.Alias().ConnectChanged(func(hasValue bool, value string) {
		if !hasValue {
			return
		}
		d.Alias = value
		logger.Debugf("%s Alias: %v", d, value)
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.Address().ConnectChanged(func(hasValue bool, value string) {
		if !hasValue {
			return
		}
		d.Address = value
		logger.Debugf("%s Address: %v", d, value)
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.Trusted().ConnectChanged(func(hasValue bool, value bool) {
		if !hasValue {
			return
		}
		d.Trusted = value
		logger.Debugf("%s Trusted: %v", d, value)
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.Paired().ConnectChanged(func(hasValue bool, value bool) {
		if !hasValue {
			return
		}
		d.Paired = value
		logger.Debugf("%s Paired: %v", d, value)
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.ServicesResolved().ConnectChanged(func(hasValue bool, value bool) {
		if !hasValue {
			return
		}
		d.ServicesResolved = value
		logger.Debugf("%s ServicesResolved: %v", d, value)
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.Icon().ConnectChanged(func(hasValue bool, value string) {
		if !hasValue {
			return
		}
		d.Icon = value
		logger.Debugf("%s Icon: %v", d, value)
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.UUIDs().ConnectChanged(func(hasValue bool, value []string) {
		if !hasValue {
			return
		}
		d.UUIDs = value
		logger.Debugf("%s UUIDs: %v", d, value)
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.RSSI().ConnectChanged(func(hasValue bool, value int16) {
		if !hasValue {
			d.RSSI = 0
			logger.Debugf("%s RSSI invalidated", d)
		} else {
			d.RSSI = value
			logger.Debugf("%s RSSI: %v", d, value)
		}
		d.notifyDevicePropertiesChanged()
	})

	_ = d.core.LegacyPairing().ConnectChanged(func(hasValue bool, value bool) {
		if !hasValue {
			return
		}
		logger.Debugf("%s LegacyPairing: %v", d, value)
	})

	_ = d.core.Blocked().ConnectChanged(func(hasValue bool, value bool) {
		if !hasValue {
			return
		}
		logger.Debugf("%s Blocked: %v", d, value)
	})
}

func (d *device) notifyConnectedChanged() {
	connectPhase := d.getConnectPhase()
	if connectPhase != connectPhaseNone {
		// connect is in progress
		logger.Debugf("%s handleNotifySend: connect is in progress", d)
		return
	}

	disconnectPhase := d.getDisconnectPhase()
	if disconnectPhase != disconnectPhaseNone {
		// disconnect is in progress
		logger.Debugf("%s handleNotifySend: disconnect is in progress", d)
		return
	}

	if d.connected {
		notifyConnected(d.Alias)
		//} else {
		//	if time.Since(d.pairingFailedTime) < 2*time.Second {
		//		return
		//	}
		//	notifyDisconnected(d.Alias)
	}
}

func (d *device) updateState() {
	newState := d.getState()
	if d.State != newState {
		d.State = newState
		logger.Debugf("%s State: %s", d, d.State)
	}
}

func (d *device) getState() deviceState {
	if d.agentWorking {
		return deviceStateConnecting
	}

	if d.connectPhase != connectPhaseNone {
		return deviceStateConnecting

	} else if d.disconnectPhase != connectPhaseNone {
		return deviceStateDisconnecting

	} else {
		if d.connected {
			return deviceStateConnected
		} else {
			return deviceStateDisconnected
		}
	}
}

func (d *device) getAddress() string {
	return d.adapter.address + "/" + d.Address
}

func (d *device) doConnect(hasNotify bool) error {
	connectPhase := d.getConnectPhase()  //获取当前的连接状态，是正在连接，还是已经连接成功。
	disconnectPhase := d.getDisconnectPhase()
	if connectPhase != connectPhaseNone {   //已经在连接中。
		logger.Warningf("%s connect is in progress", d)
		return nil
	} else if disconnectPhase != disconnectPhaseNone {  //已经在断开中。
		logger.Debugf("%s disconnect is in progress", d)
		return nil
	}

	d.setConnectPhase(connectPhaseStart)   //设置为开始连接。
	defer d.setConnectPhase(connectPhaseNone)

	err := d.cancelBlock()
	if err != nil {
		if hasNotify {
			// TODO(jouyouyun): notify device blocked
		}
		return err
	}

	err = d.doPair()
	if err != nil {
		if hasNotify {

			if d.getDisconnectPhase() == disconnectPhaseNone {
				d.core.Disconnect(0)  //已经是断开状态的话，直接切断和该设备的连接。
			} else {
				d.setDisconnectPhase(disconnectPhaseNone)  //设置为断开状态。
				d.updateState()
			}
			killBluetoothDialog()
			notifyConnectFailed(d.Alias, err.Error())
		}
		return err
	}
	killBluetoothDialog()   //会kill掉 /usr/lib/deepin-daemon/dde-bluetooth-dialog 这个进程。  不过这个进程好像没有出现。
	d.audioA2DPWorkaround() //bluez不支持同时连接多个a2dp设备。连接前，需要断开。

	err = d.doRealConnect() //进行真正的连接。
	if err != nil {
		if hasNotify {
			d.core.Disconnect(0)
			notifyConnectFailedHostDown(d.Alias)
		}
		return err
	}

	d.ConnectState = true  //连接时，设置connectstate 为true
	d.notifyDevicePropertiesChanged()
	if hasNotify && d.Paired && d.State == deviceStateConnected && d.ConnectState {
		notifyConnected(d.Alias)  //通知进行连接
	}
	return nil
}

func (d *device) doRealConnect() error {
	d.setConnectPhase(connectPhaseConnectProfilesStart)   //开始真正连接
	err := d.core.Connect(0)
	d.setConnectPhase(connectPhaseConnectProfilesEnd)	  //真正连接结束。
	if err != nil {
		// connect failed
		logger.Warningf("%s connect failed: %v", d, err)
		globalBluetooth.config.setDeviceConfigConnected(d.getAddress(), false)
		return err
	}

	// connect succeeded
	logger.Infof("%s connect succeeded", d)
	globalBluetooth.config.setDeviceConfigConnected(d.getAddress(), true)  //连接成功后，设置dc.Connected为true。

	// auto trust device when connecting success
	d.doTrust()  //连接成功后，将设备设置为信任状态。

	return nil
}

func (d *device) doTrust() error {
	trusted, _ := d.core.Trusted().Get(0)
	if trusted {
		return nil
	}
	err := d.core.Trusted().Set(0, true)
	if err != nil {
		logger.Warning(err)
	}
	return err
}

func (d *device) cancelBlock() error {
	blocked, err := d.core.Blocked().Get(0)
	if err != nil {
		logger.Warning(err)
		return err
	}
	if !blocked {
		return nil
	}
	err = d.core.Blocked().Set(0, false)  //设置为blocked 为false
	if err != nil {
		logger.Warning(err)
	}
	return err
}

func (d *device) doPair() error {
	paired, err := d.core.Paired().Get(0)
	if err != nil {
		logger.Warning(err)
		return err
	}
	if paired {  //如果已经配对成功，直接返回。
		logger.Debugf("%s already paired", d)
		return nil
	}

	d.setConnectPhase(connectPhasePairStart) //状态为开始配对
	err = d.core.Pair(0)
	d.setConnectPhase(connectPhasePairEnd)	//状态为结束配对。
	if err != nil {
		logger.Warningf("%s pair failed: %v", d, err)
		d.pairingFailedTime = time.Now()
		d.setConnectPhase(connectPhaseNone)
		return err
	}

	logger.Warningf("%s pair succeeded", d)  //配对成功
	return nil
}

func (d *device) audioA2DPWorkaround() {
	// TODO: remove work code if bluez a2dp is ok
	// bluez do not support muti a2dp devices
	// disconnect a2dp device before connect
	for _, uuid := range d.UUIDs {
		if uuid == A2DP_SINK_UUID {
			globalBluetooth.disconnectA2DPDeviceExcept(d)
		}
	}
}

func (d *device) Connect() {
	logger.Debug(d, "call Connect()")
	d.setActiveDoConnect(true)   //设置进入活跃状体，将要连接。
	d.doConnect(true) //进行连接操作。
}

func (d *device) Disconnect() {
	logger.Debugf("%s call Disconnect()", d)

	disconnectPhase := d.getDisconnectPhase()
	if disconnectPhase != disconnectPhaseNone {
		logger.Debugf("%s disconnect is in progress", d)
		return
	}

	d.setDisconnectPhase(disconnectPhaseStart)
	defer d.setDisconnectPhase(disconnectPhaseNone)

	connected, err := d.core.Connected().Get(0)
	if err != nil {
		logger.Warning(err)
		return
	}
	if !connected {
		logger.Debugf("%s not connected", d)
		return
	}

	globalBluetooth.config.setDeviceConfigConnected(d.getAddress(), false)

	ch := d.goWaitDisconnect()

	d.setDisconnectPhase(disconnectPhaseDisconnectStart)
	err = d.core.Disconnect(0)
	if err != nil {
		logger.Warningf("failed to disconnect %s: %v", d, err)
	}
	d.setDisconnectPhase(disconnectPhaseDisconnectEnd)
	d.ConnectState = false   //断开连接时，设置connectState = false。
	d.notifyDevicePropertiesChanged()

	<-ch
	notifyDisconnected(d.Alias)
}

func (d *device) goWaitDisconnect() chan struct{} {
	ch := make(chan struct{})
	go func() {
		select {
		case <-d.disconnectChan:
			logger.Debugf("%s disconnectChan receive ok", d)
		case <-time.After(60 * time.Second):
			logger.Debugf("%s disconnectChan receive timed out", d)
		}
		ch <- struct{}{}
	}()
	return ch
}

func killBluetoothDialog() {
	logger.Debug("killBluetoothDialog")
	if cmdPinDialog == nil {
		return		
	} 
	err := cmdPinDialog.Process.Kill()
	if err != nil {
		logger.Warning("kill err ", err)
	}
}

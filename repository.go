package firmwarecampaign

import (
	"sort"
	"sync"
	"time"
)

// Repository 是持久化抽象。所有方法的实现必须保证并发安全；
// 服务层依赖 UpdateTx 提供的串行化事务来保证状态单调。
type Repository interface {
	// Firmware
	PutFirmware(fw Firmware) error
	GetFirmware(id string) (Firmware, error)

	// Device
	PutDevice(d Device) error
	GetDevice(id string) (Device, error)
	ListDevices() []Device

	// Campaign
	PutCampaign(c Campaign) error
	GetCampaign(id string) (Campaign, error)
	ListCampaigns() []Campaign

	// Upgrades
	PutUpgrade(u DeviceUpgrade) error
	GetUpgrade(campaignID, deviceID string) (DeviceUpgrade, error)
	ListUpgrades(campaignID string) []DeviceUpgrade

	// Device lock: 记录设备当前所属的活动（非终态活动占用）。
	LockDevice(deviceID, campaignID string) error
	UnlockDevice(deviceID, campaignID string)
	DeviceLock(deviceID string) (campaignID string, ok bool)

	// Outbox
	AddOutbox(e *OutboxEvent) error
	ListPendingOutbox() []OutboxEvent
	MarkOutboxDispatched(id int64) error

	// Snapshot
	PutSnapshot(s CampaignSnapshot) error
	GetSnapshot(campaignID string) (CampaignSnapshot, error)

	// UpdateTx 在一个可串行化事务中执行 fn。fn 返回 error 时整体回滚。
	// fn 内通过 TxStore 读写，写入只在 fn 成功返回后可见。
	UpdateTx(fn func(tx TxStore) error) error
}

// TxStore 是事务内可见的读写接口。
type TxStore interface {
	GetFirmware(id string) (Firmware, error)
	GetDevice(id string) (Device, error)
	ListDevices() []Device
	GetCampaign(id string) (Campaign, error)
	ListCampaigns() []Campaign
	GetUpgrade(campaignID, deviceID string) (DeviceUpgrade, error)
	ListUpgrades(campaignID string) []DeviceUpgrade
	DeviceLock(deviceID string) (campaignID string, ok bool)
	GetSnapshot(campaignID string) (CampaignSnapshot, error)

	PutFirmware(fw Firmware)
	PutDevice(d Device)
	PutCampaign(c Campaign)
	PutUpgrade(u DeviceUpgrade)
	LockDevice(deviceID, campaignID string)
	UnlockDevice(deviceID, campaignID string)
	AddOutbox(e *OutboxEvent)
	PutSnapshot(s CampaignSnapshot)
}

// MemoryRepository 是基于内存、互斥锁串行化的 Repository 实现，
// 适用于测试与单机运行；接口设计与 SQL 仓储一致，可平滑替换。
type MemoryRepository struct {
	mu        sync.Mutex
	firmware  map[string]Firmware
	devices   map[string]Device
	campaigns map[string]Campaign
	upgrades  map[string]DeviceUpgrade // key: campaignID|deviceID
	locks     map[string]string        // deviceID -> campaignID
	outbox    []OutboxEvent
	snapshots map[string]CampaignSnapshot
	outboxSeq int64
}

// NewMemoryRepository 创建空的内存仓储。
func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		firmware:  map[string]Firmware{},
		devices:   map[string]Device{},
		campaigns: map[string]Campaign{},
		upgrades:  map[string]DeviceUpgrade{},
		locks:     map[string]string{},
		snapshots: map[string]CampaignSnapshot{},
	}
}

func upgradeKey(campaignID, deviceID string) string {
	return campaignID + "|" + deviceID
}

// ---- 非事务读（加锁拷贝） ----

func (r *MemoryRepository) PutFirmware(fw Firmware) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.firmware[fw.ID] = fw
	return nil
}

func (r *MemoryRepository) GetFirmware(id string) (Firmware, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fw, ok := r.firmware[id]
	if !ok {
		return Firmware{}, ErrNotFound
	}
	return fw, nil
}

func (r *MemoryRepository) PutDevice(d Device) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devices[d.ID] = d
	return nil
}

func (r *MemoryRepository) GetDevice(id string) (Device, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.devices[id]
	if !ok {
		return Device{}, ErrNotFound
	}
	return d, nil
}

func (r *MemoryRepository) ListDevices() []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *MemoryRepository) PutCampaign(c Campaign) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.campaigns[c.ID] = c
	return nil
}

func (r *MemoryRepository) GetCampaign(id string) (Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.campaigns[id]
	if !ok {
		return Campaign{}, ErrNotFound
	}
	return c, nil
}

func (r *MemoryRepository) ListCampaigns() []Campaign {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Campaign, 0, len(r.campaigns))
	for _, c := range r.campaigns {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *MemoryRepository) PutUpgrade(u DeviceUpgrade) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upgrades[upgradeKey(u.CampaignID, u.DeviceID)] = u
	return nil
}

func (r *MemoryRepository) GetUpgrade(campaignID, deviceID string) (DeviceUpgrade, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.upgrades[upgradeKey(campaignID, deviceID)]
	if !ok {
		return DeviceUpgrade{}, ErrNotFound
	}
	return u, nil
}

func (r *MemoryRepository) ListUpgrades(campaignID string) []DeviceUpgrade {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listUpgradesLocked(campaignID)
}

func (r *MemoryRepository) listUpgradesLocked(campaignID string) []DeviceUpgrade {
	out := make([]DeviceUpgrade, 0)
	for _, u := range r.upgrades {
		if u.CampaignID == campaignID {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wave != out[j].Wave {
			return out[i].Wave < out[j].Wave
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}

func (r *MemoryRepository) LockDevice(deviceID, campaignID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.locks[deviceID]; ok && owner != campaignID {
		return ErrDeviceBusy
	}
	r.locks[deviceID] = campaignID
	return nil
}

func (r *MemoryRepository) UnlockDevice(deviceID, campaignID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.locks[deviceID]; ok && owner == campaignID {
		delete(r.locks, deviceID)
	}
}

func (r *MemoryRepository) DeviceLock(deviceID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cid, ok := r.locks[deviceID]
	return cid, ok
}

func (r *MemoryRepository) AddOutbox(e *OutboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outboxSeq++
	e.ID = r.outboxSeq
	e.CreatedAt = time.Now()
	r.outbox = append(r.outbox, *e)
	return nil
}

func (r *MemoryRepository) ListPendingOutbox() []OutboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]OutboxEvent, 0)
	for _, e := range r.outbox {
		if !e.Dispatched {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *MemoryRepository) MarkOutboxDispatched(id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.outbox {
		if r.outbox[i].ID == id {
			r.outbox[i].Dispatched = true
			return nil
		}
	}
	return ErrNotFound
}

func (r *MemoryRepository) PutSnapshot(s CampaignSnapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots[s.CampaignID] = s
	return nil
}

func (r *MemoryRepository) GetSnapshot(campaignID string) (CampaignSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[campaignID]
	if !ok {
		return CampaignSnapshot{}, ErrNotFound
	}
	return s, nil
}

// ---- 事务 ----

type txState struct {
	firmware  map[string]Firmware
	devices   map[string]Device
	campaigns map[string]Campaign
	upgrades  map[string]DeviceUpgrade
	locks     map[string]string
	outbox    []OutboxEvent
	snapshots map[string]CampaignSnapshot
}

type memoryTx struct {
	repo *MemoryRepository
	st   *txState
}

func (t *memoryTx) GetFirmware(id string) (Firmware, error) {
	fw, ok := t.st.firmware[id]
	if !ok {
		return Firmware{}, ErrNotFound
	}
	return fw, nil
}

func (t *memoryTx) GetDevice(id string) (Device, error) {
	d, ok := t.st.devices[id]
	if !ok {
		return Device{}, ErrNotFound
	}
	return d, nil
}

func (t *memoryTx) ListDevices() []Device {
	out := make([]Device, 0, len(t.st.devices))
	for _, d := range t.st.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *memoryTx) GetCampaign(id string) (Campaign, error) {
	c, ok := t.st.campaigns[id]
	if !ok {
		return Campaign{}, ErrNotFound
	}
	return c, nil
}

func (t *memoryTx) ListCampaigns() []Campaign {
	out := make([]Campaign, 0, len(t.st.campaigns))
	for _, c := range t.st.campaigns {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (t *memoryTx) GetUpgrade(campaignID, deviceID string) (DeviceUpgrade, error) {
	u, ok := t.st.upgrades[upgradeKey(campaignID, deviceID)]
	if !ok {
		return DeviceUpgrade{}, ErrNotFound
	}
	return u, nil
}

func (t *memoryTx) ListUpgrades(campaignID string) []DeviceUpgrade {
	out := make([]DeviceUpgrade, 0)
	for _, u := range t.st.upgrades {
		if u.CampaignID == campaignID {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wave != out[j].Wave {
			return out[i].Wave < out[j].Wave
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}

func (t *memoryTx) DeviceLock(deviceID string) (string, bool) {
	cid, ok := t.st.locks[deviceID]
	return cid, ok
}

func (t *memoryTx) GetSnapshot(campaignID string) (CampaignSnapshot, error) {
	s, ok := t.st.snapshots[campaignID]
	if !ok {
		return CampaignSnapshot{}, ErrNotFound
	}
	return s, nil
}

func (t *memoryTx) PutFirmware(fw Firmware) { t.st.firmware[fw.ID] = fw }
func (t *memoryTx) PutDevice(d Device)      { t.st.devices[d.ID] = d }
func (t *memoryTx) PutCampaign(c Campaign)  { t.st.campaigns[c.ID] = c }
func (t *memoryTx) PutUpgrade(u DeviceUpgrade) {
	t.st.upgrades[upgradeKey(u.CampaignID, u.DeviceID)] = u
}
func (t *memoryTx) LockDevice(deviceID, campaignID string) {
	t.st.locks[deviceID] = campaignID
}
func (t *memoryTx) UnlockDevice(deviceID, campaignID string) {
	if owner, ok := t.st.locks[deviceID]; ok && owner == campaignID {
		delete(t.st.locks, deviceID)
	}
}
func (t *memoryTx) AddOutbox(e *OutboxEvent) {
	// 事务内使用负数临时 ID，提交时由仓储统一编号，保证事件 ID 单调。
	e.ID = -int64(len(t.st.outbox) + 1)
	t.st.outbox = append(t.st.outbox, *e)
}
func (t *memoryTx) PutSnapshot(s CampaignSnapshot) { t.st.snapshots[s.CampaignID] = s }

// UpdateTx 以整库互斥锁串行化执行事务：fn 看到的是快照拷贝，
// 只有 fn 返回 nil 时拷贝才整体提交（含 outbox 事件重新编号）。
func (r *MemoryRepository) UpdateTx(fn func(tx TxStore) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := &txState{
		firmware:  make(map[string]Firmware, len(r.firmware)),
		devices:   make(map[string]Device, len(r.devices)),
		campaigns: make(map[string]Campaign, len(r.campaigns)),
		upgrades:  make(map[string]DeviceUpgrade, len(r.upgrades)),
		locks:     make(map[string]string, len(r.locks)),
		snapshots: make(map[string]CampaignSnapshot, len(r.snapshots)),
	}
	for k, v := range r.firmware {
		st.firmware[k] = v
	}
	for k, v := range r.devices {
		st.devices[k] = v
	}
	for k, v := range r.campaigns {
		st.campaigns[k] = v
	}
	for k, v := range r.upgrades {
		st.upgrades[k] = v
	}
	for k, v := range r.locks {
		st.locks[k] = v
	}
	for k, v := range r.snapshots {
		st.snapshots[k] = v
	}
	st.outbox = append(st.outbox, r.outbox...)

	tx := &memoryTx{repo: r, st: st}
	if err := fn(tx); err != nil {
		return err
	}

	// 提交。
	r.firmware = st.firmware
	r.devices = st.devices
	r.campaigns = st.campaigns
	r.upgrades = st.upgrades
	r.locks = st.locks
	r.snapshots = st.snapshots
	// outbox：事务起始时是完整拷贝（含已投递事件），提交时只需要
	// 为事务内新增的（负临时 ID）事件分配最终单调 ID。
	final := st.outbox[:0:0]
	final = append(final, st.outbox...)
	for i := range final {
		if final[i].ID < 0 {
			r.outboxSeq++
			final[i].ID = r.outboxSeq
		}
	}
	r.outbox = final
	return nil
}

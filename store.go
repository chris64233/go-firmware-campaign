package firmwarecampaign

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// snapshot 是服务的全部可持久化状态（含 outbox）。
type snapshot struct {
	Firmwares map[string]*Firmware `json:"firmwares"`
	Devices   map[string]*Device   `json:"devices"`
	Campaigns map[string]*Campaign `json:"campaigns"`
	Outbox    []OutboxEvent        `json:"outbox"`
	Receipts  map[string]bool      `json:"receipts"` // 已处理回执 ID，用于去重
}

func (s *snapshot) ensure() {
	if s.Firmwares == nil {
		s.Firmwares = map[string]*Firmware{}
	}
	if s.Devices == nil {
		s.Devices = map[string]*Device{}
	}
	if s.Campaigns == nil {
		s.Campaigns = map[string]*Campaign{}
	}
	if s.Receipts == nil {
		s.Receipts = map[string]bool{}
	}
}

// Persister 持久化服务状态。Save 在每次状态变更后调用。
type Persister interface {
	Load() (*snapshot, error)
	Save(s *snapshot) error
}

// FilePersister 将状态以 JSON 快照形式写入单个文件（先写临时文件再原子改名）。
type FilePersister struct {
	path string
}

// NewFilePersister 创建一个以 path 为存储文件的持久化器。
func NewFilePersister(path string) *FilePersister {
	return &FilePersister{path: path}
}

// Load 读取快照；文件不存在时返回 (nil, nil)。
func (p *FilePersister) Load() (*snapshot, error) {
	data, err := os.ReadFile(p.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var st snapshot
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode state file: %w", err)
	}
	st.ensure()
	return &st, nil
}

// Save 原子地写入快照。
func (p *FilePersister) Save(s *snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	if err := os.Rename(tmp, p.path); err != nil {
		return fmt.Errorf("commit state file: %w", err)
	}
	return nil
}

// Dir 返回状态文件所在目录（便于调用方准备目录）。
func (p *FilePersister) Dir() string { return filepath.Dir(p.path) }

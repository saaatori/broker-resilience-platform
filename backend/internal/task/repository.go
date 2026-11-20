package task

import (
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// Repository 数据访问层：只封装 GORM 操作，不含业务规则。
type Repository struct {
	db *gorm.DB
}

// NewRepository 注入 *gorm.DB 构造 repository。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Create 写入一个测试任务（status 由 Service 置 PENDING）。
func (r *Repository) Create(t *model.TestTask) error {
	return r.db.Create(t).Error
}

// FindScenarioByKey 按场景键查询且仅返回启用场景（接口契约：禁用=不存在，建任务侧不可见）。
func (r *Repository) FindScenarioByKey(key string) (*model.TestScenario, error) {
	var sc model.TestScenario
	if err := r.db.Where("key = ? AND enabled = ?", key, true).First(&sc).Error; err != nil {
		return nil, err
	}
	return &sc, nil
}

// EnsureScenario 场景不存在则插入、存在即跳过（启动 seed 幂等）。
func (r *Repository) EnsureScenario(s *model.TestScenario) error {
	var count int64
	if err := r.db.Model(&model.TestScenario{}).Where("key = ?", s.Key).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return r.db.Create(s).Error
}

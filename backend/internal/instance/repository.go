package instance

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

// Create 写入一个实例，password 须先经 Service.EncryptPassword 加密。
func (r *Repository) Create(ins *model.BrokerInstance) error {
	return r.db.Create(ins).Error
}

// FindByID 按主键查询实例；不存在返回 gorm.ErrRecordNotFound。
func (r *Repository) FindByID(id int64) (*model.BrokerInstance, error) {
	var ins model.BrokerInstance
	if err := r.db.First(&ins, id).Error; err != nil {
		return nil, err
	}
	return &ins, nil
}

// Save 保存实例的最新状态（当前用于回写 health）。
func (r *Repository) Save(ins *model.BrokerInstance) error {
	return r.db.Save(ins).Error
}

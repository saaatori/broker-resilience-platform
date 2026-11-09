package auth

import (
	"gorm.io/gorm"

	"github.com/saaatori/broker-resilience-platform/backend/internal/model"
)

// UserRepository 数据访问层：只封装 GORM 操作，不含业务规则。
type UserRepository struct {
	db *gorm.DB
}

// NewUserRepository 注入 *gorm.DB 构造 repository。
func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

// Count 返回 sys_user 总行数，供种子管理员判定。
func (r *UserRepository) Count() (int64, error) {
	var n int64
	if err := r.db.Model(&model.SysUser{}).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// Create 写入一个用户，password 须先经 Service.HashPassword 哈希。
func (r *UserRepository) Create(u *model.SysUser) error {
	return r.db.Create(u).Error
}

// FindByUsername 按用户名查询用户；不存在时返回 gorm.ErrRecordNotFound（第 5 天登录校验用）。
func (r *UserRepository) FindByUsername(username string) (*model.SysUser, error) {
	var u model.SysUser
	if err := r.db.Where("username = ?", username).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

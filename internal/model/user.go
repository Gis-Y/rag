package model

import "time"

// User 对应于数据库中的 'users' 表
type User struct {
	ID         uint      `gorm:"primaryKey;autoIncrement"`
	Username   string    `gorm:"type:varchar(255);not null;unique"`
	Password   string    `gorm:"type:varchar(255);not null" json:"-"` // Hide password in json output
	Role       string    `gorm:"type:enum('USER', 'ADMIN');default:'USER'"`
	OrgTags    string    `gorm:"type:varchar(255)"`
	PrimaryOrg string    `gorm:"type:varchar(50)"`
	CreatedAt  time.Time `gorm:"autoCreateTime"`
	UpdatedAt  time.Time `gorm:"autoUpdateTime"`
}

// TableName 指定 GORM 使用的表名
func (User) TableName() string {
	return "users"
}

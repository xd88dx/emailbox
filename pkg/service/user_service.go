package service

import (
	"context"
	"emailbox/pkg/model"
	"emailbox/pkg/repo"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

type UserService struct{ store *repo.Store }

func NewUserService(s *repo.Store) *UserService { return &UserService{store: s} }
func (s *UserService) Get(ctx context.Context, id string) (*model.UserResponse, error) {
	u, e := s.store.GetUserByID(ctx, id)
	if e != nil {
		return nil, e
	}
	r := u.ToResponse()
	return &r, nil
}
func (s *UserService) Update(ctx context.Context, id string, req model.UpdateProfileRequest) (*model.UserResponse, error) {
	u, e := s.store.GetUserByID(ctx, id)
	if e != nil {
		return nil, e
	}
	if username := strings.TrimSpace(req.Username); username != "" {
		if err := validateUsername(username); err != nil {
			return nil, err
		}
		u.Username = username
	}
	// nil 表示没传，保持原值；空串表示用户主动清空。
	if req.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*req.Email))
		if email != "" {
			if err := validateEmail(email); err != nil {
				return nil, err
			}
		}
		u.Email = email
	}
	if e = s.store.UpdateUserProfile(ctx, u); e != nil {
		if errors.Is(e, repo.ErrConflict) {
			return nil, errors.New("用户名或邮箱已被使用")
		}
		return nil, e
	}
	r := u.ToResponse()
	return &r, nil
}

// UpdateAutoRefresh 保存收信自动刷新偏好。关闭时允许 minutes 传 0,
// 此时沿用库里已记住的间隔,避免用户下次打开时又跳回部署默认。
func (s *UserService) UpdateAutoRefresh(ctx context.Context, id string, req model.UpdateAutoRefreshRequest) (*model.UserResponse, error) {
	u, e := s.store.GetUserByID(ctx, id)
	if e != nil {
		return nil, e
	}
	minutes := u.AutoRefreshMinutes
	if req.Minutes != 0 {
		if req.Minutes < model.MinAutoRefreshMinutes || req.Minutes > model.MaxAutoRefreshMinutes {
			return nil, fmt.Errorf("自动刷新间隔需在 %d~%d 分钟之间", model.MinAutoRefreshMinutes, model.MaxAutoRefreshMinutes)
		}
		m := req.Minutes
		minutes = &m
	}
	if req.Enabled && minutes == nil {
		return nil, fmt.Errorf("开启自动刷新前请先选择间隔（%d~%d 分钟）", model.MinAutoRefreshMinutes, model.MaxAutoRefreshMinutes)
	}
	enabled := req.Enabled
	if e = s.store.UpdateUserAutoRefresh(ctx, id, &enabled, minutes); e != nil {
		return nil, e
	}
	u.AutoRefreshEnabled = &enabled
	u.AutoRefreshMinutes = minutes
	r := u.ToResponse()
	return &r, nil
}
func (s *UserService) ChangePassword(ctx context.Context, id string, req model.ChangePasswordRequest) error {
	if len(req.NewPassword) < 6 {
		return errors.New("新密码长度不能少于6个字符")
	}
	if len(req.NewPassword) > maxPasswordLen {
		return fmt.Errorf("新密码长度不能超过 %d 个字节", maxPasswordLen)
	}
	u, e := s.store.GetUserByID(ctx, id)
	if e != nil {
		return e
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.OldPassword)) != nil {
		return errors.New("原密码错误")
	}
	h, e := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if e != nil {
		return e
	}
	if e = s.store.UpdateUserPassword(ctx, id, string(h)); e != nil {
		return e
	}
	return s.store.DeleteUserSessions(ctx, id)
}

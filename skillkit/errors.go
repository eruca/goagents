package skillkit

import "errors"

var (
	ErrInvalidSkillManifest = errors.New("invalid skill manifest")
	ErrInvalidSkillResource = errors.New("invalid skill resource")
	ErrSkillNotFound        = errors.New("skill not found")
	ErrSkillAmbiguous       = errors.New("skill name is ambiguous")
)

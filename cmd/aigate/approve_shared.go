package main

import "aigate/internal/approval/ui"

// The approve screen and its safety checks live in internal/approval/ui (shared
// with `orch approve`). These aliases keep the names this package's tests use.
const (
	beginMark = ui.BeginMark
	endMark   = ui.EndMark

	reasonUnsafeBytes         = ui.ReasonUnsafeBytes
	reasonTextBecameBinary    = ui.ReasonTextBecameBinary
	reasonBinaryBecameText    = ui.ReasonBinaryBecameText
	reasonBinaryExecutable    = ui.ReasonBinaryExecutable
	reasonBinarySource        = ui.ReasonBinarySource
	reasonBinaryUnknown       = ui.ReasonBinaryUnknown
	reasonBinaryLoadable      = ui.ReasonBinaryLoadable
	reasonBinaryMagicMismatch = ui.ReasonBinaryMagicMismatch
	reasonBinaryInLoadedDir   = ui.ReasonBinaryInLoadedDir
	reasonPathCaseCollision   = ui.ReasonPathCaseCollision
)

var (
	binaryRefusal     = ui.BinaryRefusal
	unsafeApproveText = ui.UnsafeApproveText
	summaryRefusal    = ui.SummaryRefusal
	inLoadedDir       = ui.InLoadedDir
	hasBinDir         = ui.HasBinDir
	isLoadableCode    = ui.IsLoadableCode
	loadedCodeDirs    = ui.LoadedCodeDirs
	binaryMagic       = ui.BinaryMagic
	magiclessExt      = ui.MagiclessExt
	binaryAllowExt    = ui.BinaryAllowExt
	textLikeExt       = ui.TextLikeExt
	textLikeName      = ui.TextLikeName
)

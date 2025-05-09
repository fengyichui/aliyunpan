// Copyright (c) 2020 tickstep.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package panupload

import (
	"errors"
	"fmt"
	"github.com/tickstep/aliyunpan/internal/log"
	"github.com/tickstep/aliyunpan/internal/plugins"
	"github.com/tickstep/library-go/logger"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tickstep/aliyunpan/internal/utils"
	"github.com/tickstep/library-go/requester/rio/speeds"

	"github.com/tickstep/aliyunpan-api/aliyunpan"
	"github.com/tickstep/aliyunpan-api/aliyunpan/apierror"
	"github.com/tickstep/aliyunpan/internal/config"
	"github.com/tickstep/aliyunpan/internal/file/uploader"
	"github.com/tickstep/aliyunpan/internal/functions"
	"github.com/tickstep/aliyunpan/internal/localfile"
	"github.com/tickstep/aliyunpan/internal/taskframework"
	"github.com/tickstep/library-go/converter"
	"github.com/tickstep/library-go/requester/rio"
)

const (
	// DefaultCheckPreHashFileSize PreHash计算文件大小门限，默认100MB以上文件才计算
	DefaultCheckPreHashFileSize = 100 * 1024 * 1024
)

type (
	// StepUpload 上传步骤
	StepUpload int

	// UploadTaskUnit 上传的任务单元
	UploadTaskUnit struct {
		LocalFileChecksum *localfile.LocalFileEntity // 要上传的本地文件详情
		Step              StepUpload
		SavePath          string // 保存路径
		DriveId           string // 网盘ID，例如：文件网盘，相册网盘
		FolderCreateMutex *sync.Mutex

		PanClient         *config.PanClient
		UploadingDatabase *UploadingDatabase // 数据库
		Parallel          int
		NoRapidUpload     bool  // 禁用秒传，无需计算SHA1，直接上传
		BlockSize         int64 // 分片大小

		UploadStatistic *UploadStatistic

		taskInfo *taskframework.TaskInfo
		panDir   string
		panFile  string
		state    *uploader.InstanceState

		ShowProgress   bool
		IsOverwrite    bool // 覆盖已存在的文件，如果同名文件已存在则移到回收站里
		IsSkipSameName bool // 跳过已存在的文件，即使文件内容不一致(不检查SHA1)

		// 全局速度统计
		GlobalSpeedsStat *speeds.Speeds

		// 上传文件记录器
		FileRecorder *log.FileRecorder
	}
)

const (
	// StepUploadInit 初始化步骤
	StepUploadInit StepUpload = iota
	// StepUploadPrepareUpload 上传前准备，创建上传任务
	StepUploadPrepareUpload
	// StepUploadRapidUpload 秒传步骤
	StepUploadRapidUpload
	// StepUploadUpload 正常上传步骤
	StepUploadUpload
)

const (
	StrUploadFailed = "上传文件失败"
)

func (utu *UploadTaskUnit) SetTaskInfo(taskInfo *taskframework.TaskInfo) {
	utu.taskInfo = taskInfo
}

// prepareFile 解析文件准备阶段
func (utu *UploadTaskUnit) prepareFile() {
	// 解析文件保存路径
	var (
		panDir, panFile = path.Split(utu.SavePath)
	)
	utu.panDir = path.Clean(panDir)
	utu.panFile = panFile

	// 检测断点续传
	utu.state = utu.UploadingDatabase.Search(&utu.LocalFileChecksum.LocalFileMeta)
	if utu.state != nil || utu.LocalFileChecksum.LocalFileMeta.UploadOpEntity != nil { // 读取到了上一次上传task请求的fileId
		utu.Step = StepUploadUpload
	}

	if utu.LocalFileChecksum.UploadOpEntity == nil {
		utu.Step = StepUploadPrepareUpload
		return
	}

	if utu.NoRapidUpload {
		utu.Step = StepUploadUpload
		return
	}

	//if utu.LocalFileChecksum.Length > MaxRapidUploadSize {
	//	fmt.Printf("[%s] 文件超过20GB, 无法使用秒传功能, 跳过秒传...\n", utu.taskInfo.Id())
	//	utu.Step = StepUploadUpload
	//	return
	//}

	// 下一步: 秒传
	utu.Step = StepUploadRapidUpload
}

// rapidUpload 执行秒传
func (utu *UploadTaskUnit) rapidUpload() (isContinue bool, result *taskframework.TaskUnitRunResult) {
	utu.Step = StepUploadRapidUpload

	// 是否可以秒传
	result = &taskframework.TaskUnitRunResult{}
	fmt.Printf("[%s] %s 检测秒传中, 请稍候...\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
	if utu.LocalFileChecksum.UploadOpEntity.RapidUpload {
		fmt.Printf("[%s] %s 秒传成功, 保存到网盘路径: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utu.SavePath)
		result.Succeed = true
		return false, result
	} else {
		fmt.Printf("[%s] %s 秒传失败，开始正常上传文件\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
		result.Succeed = false
		result.ResultMessage = "文件未曾上传，无法秒传"
		return true, result
	}
}

// upload 上传文件
func (utu *UploadTaskUnit) upload() (result *taskframework.TaskUnitRunResult) {
	utu.Step = StepUploadUpload

	// 创建分片上传器
	// 阿里云盘默认就是分片上传，每一个分片对应一个part_info
	// 但是不支持分片同时上传，必须单线程，并且按照顺序从1开始一个一个上传
	muer := uploader.NewMultiUploader(
		NewPanUpload(utu.PanClient, utu.SavePath, utu.DriveId, utu.LocalFileChecksum.UploadOpEntity),
		rio.NewFileReaderAtLen64(utu.LocalFileChecksum.GetFile()), &uploader.MultiUploaderConfig{
			Parallel:  utu.Parallel,
			BlockSize: utu.BlockSize,
			MaxRate:   config.Config.MaxUploadRate,
		}, utu.LocalFileChecksum.UploadOpEntity, utu.PanClient, utu.GlobalSpeedsStat)

	// 设置断点续传
	if utu.state != nil {
		muer.SetInstanceState(utu.state)
	}

	muer.OnUploadStatusEvent(func(status uploader.Status, updateChan <-chan struct{}) {
		select {
		case <-updateChan:
			utu.UploadingDatabase.UpdateUploading(&utu.LocalFileChecksum.LocalFileMeta, muer.InstanceState())
			utu.UploadingDatabase.Save()
		default:
		}

		if utu.ShowProgress {
			// 如果上传速度为0, 剩余时间未知, 则用 - 代替
			var leftStr string
			left := status.TimeLeft()
			if left < 0 {
				leftStr = "-"
			} else {
				leftStr = left.String()
			}
			uploadedPercentage := fmt.Sprintf("%.2f%%", float64(status.Uploaded())/float64(status.TotalSize())*100)
			fmt.Printf("\r[%s] ↑ %s/%s(%s) %s/s(%s/s) in %s, left %s ............", utu.taskInfo.Id(),
				converter.ConvertFileSize(status.Uploaded(), 2),
				converter.ConvertFileSize(status.TotalSize(), 2),
				uploadedPercentage,
				converter.ConvertFileSize(status.SpeedsPerSecond(), 2),
				converter.ConvertFileSize(utu.GlobalSpeedsStat.GetSpeeds(), 2),
				status.TimeElapsed(),
				leftStr,
			)
		}
	})

	// result
	result = &taskframework.TaskUnitRunResult{}
	muer.OnSuccess(func() {
		fmt.Printf("\n")
		fmt.Printf("[%s] %s 上传文件成功, 保存到网盘路径: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utu.SavePath)
		// 统计
		utu.UploadStatistic.AddTotalSize(utu.LocalFileChecksum.Length)
		utu.UploadingDatabase.Delete(&utu.LocalFileChecksum.LocalFileMeta) // 删除
		utu.UploadingDatabase.Save()
		result.Succeed = true
	})
	muer.OnError(func(err error) {
		apiError, ok := err.(*apierror.ApiError)
		if !ok {
			// 未知错误类型 (非预期的)
			// 不重试
			result.ResultMessage = "上传文件错误"
			result.Err = err
			return
		}

		// 默认需要重试
		result.NeedRetry = true

		switch apiError.ErrCode() {
		default:
			result.ResultMessage = StrUploadFailed
			result.NeedRetry = false
			result.Err = apiError
		}
		return
	})
	er := muer.Execute()
	if er != nil {
		result.ResultMessage = StrUploadFailed
		result.NeedRetry = true
		if errors.Is(er, uploader.UploadNoSuchUpload) {
			// do not need retry
			result.NeedRetry = false
		}
		result.Err = er
	}
	return
}

func (utu *UploadTaskUnit) OnRetry(lastRunResult *taskframework.TaskUnitRunResult) {
	// 输出错误信息
	if lastRunResult.Err == nil {
		// result中不包含Err, 忽略输出
		fmt.Printf("[%s] %s, 重试 %d/%d\n", utu.taskInfo.Id(), lastRunResult.ResultMessage, utu.taskInfo.Retry(), utu.taskInfo.MaxRetry())
		return
	}
	fmt.Printf("[%s] %s, %s, 重试 %d/%d\n", utu.taskInfo.Id(), lastRunResult.ResultMessage, lastRunResult.Err, utu.taskInfo.Retry(), utu.taskInfo.MaxRetry())
}

func (utu *UploadTaskUnit) OnSuccess(lastRunResult *taskframework.TaskUnitRunResult) {
	// 执行插件
	utu.pluginCallback("success")

	// 上传文件数据记录
	if config.Config.FileRecordConfig == "1" {
		utu.FileRecorder.Append(&log.FileRecordItem{
			Status:   "成功",
			TimeStr:  utils.NowTimeStr(),
			FileSize: utu.LocalFileChecksum.LocalFileMeta.Length,
			FilePath: utu.LocalFileChecksum.Path.LogicPath,
		})
	}
}

func (utu *UploadTaskUnit) OnFailed(lastRunResult *taskframework.TaskUnitRunResult) {
	// 失败
	utu.pluginCallback("fail")
}

func (utu *UploadTaskUnit) pluginCallback(result string) {
	if utu.LocalFileChecksum == nil {
		return
	}
	pluginManger := plugins.NewPluginManager(config.GetPluginDir())
	plugin, _ := pluginManger.GetPlugin()
	_, fileName := filepath.Split(utu.LocalFileChecksum.Path.LogicPath)
	pluginParam := &plugins.UploadFileFinishParams{
		LocalFilePath:      utu.LocalFileChecksum.Path.LogicPath,
		LocalFileName:      fileName,
		LocalFileSize:      utu.LocalFileChecksum.LocalFileMeta.Length,
		LocalFileType:      "file",
		LocalFileUpdatedAt: time.Unix(utu.LocalFileChecksum.LocalFileMeta.ModTime, 0).Format("2006-01-02 15:04:05"),
		LocalFileSha1:      utu.LocalFileChecksum.LocalFileMeta.SHA1,
		UploadResult:       result,
		DriveId:            utu.DriveId,
		DriveFilePath:      utu.panDir + "/" + utu.panFile,
	}
	if er := plugin.UploadFileFinishCallback(plugins.GetContext(config.Config.ActiveUser()), pluginParam); er != nil {
		logger.Verboseln("插件UploadFileFinishCallback调用失败： {}", er)
	} else {
		logger.Verboseln("插件UploadFileFinishCallback调用成功")
	}
}

func (utu *UploadTaskUnit) OnComplete(lastRunResult *taskframework.TaskUnitRunResult) {
	// 任务结束，可能成功也可能失败
}
func (utu *UploadTaskUnit) OnCancel(lastRunResult *taskframework.TaskUnitRunResult) {

}
func (utu *UploadTaskUnit) RetryWait() time.Duration {
	return functions.RetryWait(utu.taskInfo.Retry())
}

func (utu *UploadTaskUnit) Run() (result *taskframework.TaskUnitRunResult) {
	err := utu.LocalFileChecksum.OpenPath()
	if err != nil {
		fmt.Printf("[%s] 文件不可读, 错误信息: %s, 跳过...\n", utu.taskInfo.Id(), err)
		return
	}
	defer utu.LocalFileChecksum.Close() // 关闭文件

	timeStart := time.Now()
	result = &taskframework.TaskUnitRunResult{}

	fmt.Printf("[%s] %s 准备上传: %s => %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utu.LocalFileChecksum.Path.LogicPath, utu.SavePath)

	defer func() {
		var msg string
		if result.Err != nil {
			msg = "失败！" + result.ResultMessage + "," + result.Err.Error()
		} else if result.Succeed {
			msg = "成功！" + result.ResultMessage
		} else {
			msg = result.ResultMessage
		}
		fmt.Printf("[%s] %s 文件上传结果： %s 耗时 %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), msg, utils.ConvertTime(time.Now().Sub(timeStart)))
	}()

	// 准备文件
	utu.prepareFile()
	logger.Verbosef("[%s] %s 准备结束, 准备耗时 %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utils.ConvertTime(time.Now().Sub(timeStart)))

	var apierr *apierror.ApiError
	var rs *aliyunpan.MkdirResult
	var efi *aliyunpan.FileEntity
	var appCreateUploadFileParam *aliyunpan.CreateFileUploadParam
	var sha1Str string
	var contentHashName string
	var checkNameMode string
	var saveFilePath string
	var uploadOpEntity *aliyunpan.CreateFileUploadResult
	var proofCode = ""
	var localFileInfo os.FileInfo
	var localFile *os.File
	var newBlockSize int64

	switch utu.Step {
	case StepUploadPrepareUpload:
		goto StepUploadPrepareUpload
	case StepUploadRapidUpload:
		goto stepUploadRapidUpload
	case StepUploadUpload:
		goto stepUploadUpload
	}

StepUploadPrepareUpload:
	// 创建上传任务
	// 创建云盘文件夹
	saveFilePath = path.Dir(utu.SavePath)
	if saveFilePath != "/" {
		utu.FolderCreateMutex.Lock()
		fmt.Printf("[%s] %s 正在检测和创建云盘文件夹: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), saveFilePath)
		fe, apierr1 := utu.PanClient.OpenapiPanClient().FileInfoByPath(utu.DriveId, saveFilePath)
		time.Sleep(1 * time.Second)
		needToCreateFolder := false
		if apierr1 != nil && apierr1.Code == apierror.ApiCodeFileNotFoundCode {
			needToCreateFolder = true
		} else {
			if fe == nil {
				needToCreateFolder = true
			} else {
				rs = &aliyunpan.MkdirResult{}
				rs.FileId = fe.FileId
			}
		}
		if needToCreateFolder {
			logger.Verbosef("[%s] %s 创建云盘文件夹: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), saveFilePath)
			rs, apierr = utu.PanClient.OpenapiPanClient().MkdirByFullPath(utu.DriveId, saveFilePath)
			if apierr != nil || rs.FileId == "" {
				result.Err = apierr
				result.ResultMessage = "创建云盘文件夹失败"
				utu.FolderCreateMutex.Unlock()
				return
			}
			logger.Verbosef("[%s] %s 创建云盘文件夹成功\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
		}
		utu.FolderCreateMutex.Unlock()
	} else {
		rs = &aliyunpan.MkdirResult{}
		rs.FileId = ""
	}
	time.Sleep(time.Duration(2) * time.Second)

	sha1Str = ""
	proofCode = ""
	contentHashName = "sha1"
	checkNameMode = "auto_rename"
	// 如果启用了 覆盖/跳过 已存在的文件,则需要提前检查文件是否存在
	if utu.IsOverwrite || utu.IsSkipSameName {
		efi, apierr = utu.PanClient.OpenapiPanClient().FileInfoByPath(utu.DriveId, utu.SavePath)
		if apierr != nil && apierr.Code != apierror.ApiCodeFileNotFoundCode {
			result.Err = apierr
			result.ResultMessage = "检测同名文件失败"
			return
		}
	}
	if utu.IsSkipSameName {
		if efi != nil && efi.FileId != "" {
			result.Succeed = true
			result.Extra = efi
			fmt.Printf("[%s] %s 检测到同名文件，跳过上传: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utu.SavePath)
			return
		}
	}
	if !utu.NoRapidUpload {
		// 正常上传流程，检测是否能秒传
		preHashMatch := true
		if utu.LocalFileChecksum.Length >= DefaultCheckPreHashFileSize {
			// 大文件，先计算 PreHash，用于检测是否可能支持秒传
			preHash := CalcFilePreHash(utu.LocalFileChecksum.Path.RealPath)
			if len(preHash) > 0 {
				if b, er := utu.PanClient.OpenapiPanClient().CheckUploadFilePreHash(&aliyunpan.FileUploadCheckPreHashParam{
					DriveId:      utu.DriveId,
					Name:         filepath.Base(utu.SavePath),
					Size:         utu.LocalFileChecksum.Length,
					ParentFileId: rs.FileId,
					PreHash:      preHash,
				}); er == nil {
					preHashMatch = b
				}
			}
		}

		if preHashMatch { // preHashMatch为true，代表该文件可能已经被上传过，能够支持秒传，所以需要进一步计算完整SHA1进行检测是否能秒传
			// 计算完整文件SHA1
			fmt.Printf("[%s] %s 正在计算文件SHA1: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utu.LocalFileChecksum.Path.LogicPath)
			utu.LocalFileChecksum.Sum(localfile.CHECKSUM_SHA1)
			sha1Str = utu.LocalFileChecksum.SHA1
			if utu.LocalFileChecksum.Length == 0 {
				sha1Str = aliyunpan.DefaultZeroSizeFileContentHash
			}

			// proof code
			localFile, _ = os.Open(utu.LocalFileChecksum.Path.RealPath)
			localFileInfo, _ = localFile.Stat()
			proofCode = aliyunpan.CalcProofCode(utu.PanClient.OpenapiPanClient().GetAccessToken(), rio.NewFileReaderAtLen64(localFile), localFileInfo.Size())
			localFile.Close()
		} else {
			// 无需计算 sha1，直接上传
			logger.Verboseln("PreHash not match, upload file directly")
			sha1Str = ""
			contentHashName = ""
			checkNameMode = "auto_rename"
		}
	} else {
		fmt.Printf("[%s] %s 已经禁用秒传检测，直接上传\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
		sha1Str = ""
		contentHashName = ""
		checkNameMode = "auto_rename"
	}

	if utu.IsOverwrite {
		// 标记覆盖旧同名文件
		// 检查同名文件是否存在
		if efi != nil && efi.FileId != "" {
			if strings.ToUpper(efi.ContentHash) == strings.ToUpper(sha1Str) {
				result.Succeed = true
				result.Extra = efi
				fmt.Printf("[%s] %s 检测到同名文件，文件内容完全一致，无需重复上传: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utu.SavePath)
				return
			}
			// existed, delete it
			var fileDeleteResult *aliyunpan.FileBatchActionResult
			var err *apierror.ApiError
			fileDeleteResult, err = utu.PanClient.OpenapiPanClient().FileDelete(&aliyunpan.FileBatchActionParam{DriveId: efi.DriveId, FileId: efi.FileId})
			if err != nil || !fileDeleteResult.Success {
				result.Err = err
				result.ResultMessage = "无法删除文件，请稍后重试"
				return
			}
			time.Sleep(time.Duration(500) * time.Millisecond)
			fmt.Printf("[%s] %s 检测到同名文件，文件内容不一致，已将旧文件移动到回收站: %s\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"), utu.SavePath)
		}
	}

	// 自动调整BlockSize大小
	newBlockSize = utils.ResizeUploadBlockSize(utu.LocalFileChecksum.Length, utu.BlockSize)
	if newBlockSize != utu.BlockSize {
		logger.Verboseln("resize upload block size to: " + converter.ConvertFileSize(newBlockSize, 2))
		utu.BlockSize = newBlockSize
	}

	// 创建上传任务
	if sha1Str != "" {
		// 计算SHA1和ProofCode，该方式支持秒传文件
		appCreateUploadFileParam = &aliyunpan.CreateFileUploadParam{
			DriveId:         utu.DriveId,
			Name:            filepath.Base(utu.SavePath),
			Size:            utu.LocalFileChecksum.Length,
			CheckNameMode:   checkNameMode,
			ParentFileId:    rs.FileId,
			BlockSize:       utu.BlockSize,
			ContentHash:     sha1Str,
			ContentHashName: contentHashName,
			ProofCode:       proofCode,
			ProofVersion:    "v1",
			LocalCreatedAt:  utils.UnixTime2LocalFormatStr(utu.LocalFileChecksum.ModTime),
			LocalModifiedAt: utils.UnixTime2LocalFormatStr(utu.LocalFileChecksum.ModTime),
		}
	} else {
		// 不支持秒传，不计算SHA1，直接上传文件
		appCreateUploadFileParam = &aliyunpan.CreateFileUploadParam{
			DriveId:         utu.DriveId,
			Name:            filepath.Base(utu.SavePath),
			Size:            utu.LocalFileChecksum.Length,
			CheckNameMode:   checkNameMode,
			ParentFileId:    rs.FileId,
			BlockSize:       utu.BlockSize,
			LocalCreatedAt:  utils.UnixTime2LocalFormatStr(utu.LocalFileChecksum.ModTime),
			LocalModifiedAt: utils.UnixTime2LocalFormatStr(utu.LocalFileChecksum.ModTime),
		}
	}

	uploadOpEntity, apierr = utu.PanClient.OpenapiPanClient().CreateUploadFile(appCreateUploadFileParam)
	if apierr != nil {
		result.Err = apierr
		result.ResultMessage = "创建上传任务失败：" + apierr.Error()
		if apierr.Code == apierror.ApiCodeTooManyRequests || apierr.Code == apierror.ApiCodeBadGateway {
			logger.Verboseln("create upload file error: " + result.ResultMessage)
			// 重试
			result.NeedRetry = true
		} else if apierr.Code == apierror.ApiCodeUploadPayloadTooLarge {
			fmt.Printf("[%s] %s 上传文件的大小超出限制，你可能需要开通阿里云盘三方权益包以便上传大文件\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
		}
		return
	}

	utu.LocalFileChecksum.UploadOpEntity = uploadOpEntity
	utu.LocalFileChecksum.ParentFolderId = rs.FileId

stepUploadRapidUpload:
	// 秒传
	if !utu.NoRapidUpload {
		isContinue, rapidUploadResult := utu.rapidUpload()
		if !isContinue {
			// 秒传成功, 返回秒传的结果
			return rapidUploadResult
		}
	}

stepUploadUpload:
	// 正常上传流程
	uploadResult := utu.upload()
	if uploadResult != nil && uploadResult.Err != nil {
		// 处理上传错误
		if errors.Is(uploadResult.Err, uploader.UploadPartNotSeq) {
			// 分片乱序错误
			if ee := utu.amendFileUploadPartNum(); ee != nil {
				// 修正分片乱序失败，先令上传任务直接失败
				logger.Verboseln("WARNING! amend uploaded parts num failed")
				fmt.Printf("[%s] %s 无法修正上传分片乱序的错误，建议重新上传\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
				uploadResult = &taskframework.TaskUnitRunResult{
					Succeed:       false,
					NeedRetry:     false,
					Cancel:        false,
					Err:           ee,
					ResultCode:    0,
					ResultMessage: "",
					Extra:         nil,
				}
				return uploadResult
			}
			goto stepUploadUpload
		}
		if errors.Is(uploadResult.Err, uploader.UploadNoSuchUpload) {
			// 上传任务过期
			fmt.Printf("[%s] %s 网盘上传任务不存在，创建新任务重新上传文件\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
			// 需要重新从0开始上传
			uploadResult = nil
			utu.LocalFileChecksum.UploadOpEntity = nil
			utu.state = nil
			goto StepUploadPrepareUpload
		}
		var apier *apierror.ApiError
		if errors.As(uploadResult.Err, &apier) {
			// 上传任务过期
			if apier.Code == apierror.ApiCodeUploadIdNotFound {
				fmt.Printf("[%s] %s 网盘上传任务已失效，创建新任务重新上传文件\n", utu.taskInfo.Id(), time.Now().Format("2006-01-02 15:04:06"))
				uploadResult = nil
				utu.LocalFileChecksum.UploadOpEntity = nil
				utu.state = nil
				goto StepUploadPrepareUpload
			}
		}
	}
	return uploadResult
}

// amendFileUploadPartNum 修正文件分片上传顺序错误
func (utu *UploadTaskUnit) amendFileUploadPartNum() error {
	if utu.LocalFileChecksum.LocalFileMeta.UploadOpEntity == nil || utu.state == nil {
		return nil
	}
	logger.Verbosef("adjust the uploaded parts num error\n")
	// 分片出现乱序
	// 获取的已上传分片信息，修正正确的分片顺序
	uploadedParts, uper := utu.PanClient.OpenapiPanClient().GetUploadedPartInfoAllItem(&aliyunpan.GetUploadedPartsParam{
		DriveId:  utu.LocalFileChecksum.LocalFileMeta.UploadOpEntity.DriveId,
		FileId:   utu.LocalFileChecksum.LocalFileMeta.UploadOpEntity.FileId,
		UploadId: utu.LocalFileChecksum.LocalFileMeta.UploadOpEntity.UploadId,
	})
	if uper != nil {
		logger.Verbosef("get uploaded parts info error: %+v\n", uper)
		return uper
	}
	// 获取最后上传的分片编号
	lastUploadedPartNum := -1
	if len(uploadedParts.UploadedParts) > 0 {
		lastUploadedPartNum = uploadedParts.UploadedParts[len(uploadedParts.UploadedParts)-1].PartNumber
	} else {
		logger.Verbosef("get uploaded parts list is empty\n")
		return errors.New("uploaded parts list is empty")
	}
	// 修正分片上传的标识
	if lastUploadedPartNum > 0 {
		logger.Verbosef("get the right uploaded parts num: %d\n", lastUploadedPartNum)
		for _, w := range utu.state.BlockList {
			if (w.ID + 1) <= lastUploadedPartNum { // 分片的编号从1开始，BlockList的id是从0开始
				w.UploadDone = true
			} else {
				w.UploadDone = false
			}
		}
	}
	return nil
}

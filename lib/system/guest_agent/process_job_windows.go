//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func attachProcessJob(ctx context.Context, process *os.Process, timeout time.Duration) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}

	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(process.Pid),
	)
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("open process for job assignment: %w", err)
	}
	defer windows.CloseHandle(processHandle)
	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("assign process to job object: %w", err)
	}
	if err := resumeProcess(process.Pid); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	done := make(chan struct{})
	var doneOnce sync.Once
	var closeOnce sync.Once
	closeJob := func() {
		closeOnce.Do(func() { _ = windows.CloseHandle(job) })
	}
	cleanup := func() {
		doneOnce.Do(func() { close(done) })
		closeJob()
	}
	go func() {
		var timeoutC <-chan time.Time
		if timeout > 0 {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			timeoutC = timer.C
		}
		select {
		case <-ctx.Done():
			_ = windows.TerminateJobObject(job, 124)
			closeJob()
		case <-timeoutC:
			_ = windows.TerminateJobObject(job, 124)
			closeJob()
		case <-done:
		}
	}()
	return cleanup, nil
}

func resumeProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("list suspended process threads: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("read suspended process threads: %w", err)
	}
	for {
		if entry.OwnerProcessID == uint32(pid) {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open suspended process thread: %w", err)
			}
			_, resumeErr := windows.ResumeThread(thread)
			windows.CloseHandle(thread)
			if resumeErr != nil {
				return fmt.Errorf("resume process thread: %w", resumeErr)
			}
			return nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return fmt.Errorf("read suspended process threads: %w", err)
		}
	}
	return fmt.Errorf("suspended process thread not found")
}

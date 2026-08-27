package diagnostics

func defaultLogKeywords() []string {
	return []string{"error", "panic", "exception", "fatal", "oom", "killed"}
}

func defaultHealthOptions() HealthOptions {
	return HealthOptions{
		LogTail:          100,
		RestartThreshold: 3,
		Keywords:         defaultLogKeywords(),
		MaxLogBytes:      defaultMaxLogBytes,
		MaxTotalLogBytes: defaultMaxTotalLogBytes,
	}
}

func defaultLogsScanOptions() LogsScanOptions {
	return LogsScanOptions{
		Tail:             500,
		Context:          0,
		Keywords:         defaultLogKeywords(),
		MaxLogBytes:      defaultMaxLogBytes,
		MaxTotalLogBytes: defaultMaxTotalLogBytes,
	}
}

func defaultVolumeOptions() VolumeOptions {
	return VolumeOptions{
		SizeMode:  volumeSizeModeAPI,
		SizeImage: volumeDefaultSizeImage,
	}
}

func defaultReportAllOptions() ReportAllOptions {
	return ReportAllOptions{
		LogTail:          200,
		LogKeywords:      defaultLogKeywords(),
		MaxLogBytes:      defaultMaxLogBytes,
		MaxTotalLogBytes: defaultMaxTotalLogBytes,
		VolumeSizeMode:   volumeSizeModeAPI,
		VolumeSizeImage:  volumeDefaultSizeImage,
	}
}

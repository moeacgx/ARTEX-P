"use client";

import { useRef, useState } from "react";

import { DownloadIcon, LoaderCircleIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";

export function useSkillDownload() {
  const active = useRef(new Set<string>());
  const [downloadingSkills, setDownloadingSkills] = useState(new Set<string>());

  async function downloadSkill(name: string) {
    // 不同入口共享锁，连续点击时也只发起一次下载。
    if (active.current.has(name)) return;
    active.current.add(name);
    setDownloadingSkills(new Set(active.current));
    try {
      await api.downloadSkill(name);
    } catch (error) {
      toast.error(`打包下载失败：${error instanceof Error ? error.message : "请稍后重试"}`);
    } finally {
      active.current.delete(name);
      setDownloadingSkills(new Set(active.current));
    }
  }

  return { downloadSkill, downloadingSkills };
}

export function SkillDownloadButton({
  name,
  downloading,
  onDownload,
}: {
  name: string;
  downloading: boolean;
  onDownload: (name: string) => Promise<void>;
}) {
  return (
    <Button
      size="sm"
      variant="outline"
      disabled={downloading}
      title="将整个技能目录打包为 ZIP；仅包含已保存的内容"
      onClick={() => void onDownload(name)}
    >
      {downloading ? <LoaderCircleIcon className="size-3.5 animate-spin" /> : <DownloadIcon className="size-3.5" />}
      {downloading ? "打包下载中…" : "打包下载"}
    </Button>
  );
}

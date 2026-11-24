<script setup lang="ts">
// 应用外壳：登录（API Token）→ 五个工作区（邮件 / 账号 / Webhook / 仪表盘 / 设置），靠 n-menu 切换。
import { computed, ref } from 'vue'
import {
  NConfigProvider, NMessageProvider, NDialogProvider, NLayout, NLayoutHeader, NLayoutContent,
  NMenu, NInput, NButton, NCard, NSpace, NText, zhCN, dateZhCN,
} from 'naive-ui'
import { type MenuOption } from 'naive-ui'
import { clearToken, request, setToken, token } from './api'
import MailView from './views/MailView.vue'
import AccountsView from './views/AccountsView.vue'
import DashboardView from './views/DashboardView.vue'
import WebhookView from './views/WebhookView.vue'
import SettingsView from './views/SettingsView.vue'

const authed = ref(token !== '')
const loginToken = ref('')
const loggingIn = ref(false)
const loginError = ref('')
const view = ref('mail')

// 401 事件：任何请求未授权 → 回到登录页
window.addEventListener('mailhub-unauthorized', () => { authed.value = false })

async function login() {
  if (!loginToken.value.trim()) return
  loggingIn.value = true
  loginError.value = ''
  try {
    setToken(loginToken.value.trim())
    await request('GET', '/api/v1/status') // 验证 token
    authed.value = true
  } catch (e) {
    loginError.value = (e as Error).message
    clearToken()
  } finally {
    loggingIn.value = false
  }
}

function logout() {
  clearToken()
  authed.value = false
  loginToken.value = ''
}

const views = {
  mail: MailView, accounts: AccountsView, webhook: WebhookView,
  dashboard: DashboardView, settings: SettingsView,
}
const current = computed(() => views[view.value])

const menuOptions: MenuOption[] = [
  { label: '邮件', key: 'mail' },
  { label: '账号', key: 'accounts' },
  { label: 'Webhook', key: 'webhook' },
  { label: '仪表盘', key: 'dashboard' },
  { label: '设置', key: 'settings' },
]
</script>

<template>
  <n-config-provider :locale="zhCN" :date-locale="dateZhCN">
    <n-message-provider>
      <n-dialog-provider>
        <!-- 登录 -->
        <div v-if="!authed" class="login-wrap">
          <n-card title="MailHub" class="login-card">
            <n-space vertical>
              <n-text depth="3">输入 API Token（环境变量 MAILHUB_API_TOKEN）</n-text>
              <n-input
                v-model:value="loginToken" type="password" show-password-on="click"
                placeholder="API Token" @keyup.enter="login"
              />
              <n-text v-if="loginError" type="error">{{ loginError }}</n-text>
              <n-button type="primary" block :loading="loggingIn" @click="login">登录</n-button>
            </n-space>
          </n-card>
        </div>

        <!-- 主界面 -->
        <n-layout v-else style="height: 100vh">
          <n-layout-header bordered class="header">
            <div class="brand">
              <span class="logo">✉</span> MailHub
            </div>
            <n-menu v-model:value="view" mode="horizontal" :options="menuOptions" />
            <div class="header-right">
              <n-button quaternary size="small" @click="logout">退出</n-button>
            </div>
          </n-layout-header>
          <n-layout-content content-style="height: calc(100vh - 61px); overflow: hidden;">
            <component :is="current" :key="view" />
          </n-layout-content>
        </n-layout>
      </n-dialog-provider>
    </n-message-provider>
  </n-config-provider>
</template>

<style>
html, body, #app { height: 100%; margin: 0; }
body { font-family: -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif; }
.login-wrap {
  height: 100vh; display: flex; align-items: center; justify-content: center;
  background: #f3f5f7;
}
.login-card { width: 360px; }
.header {
  display: flex; align-items: center; padding: 0 20px; gap: 24px; height: 60px;
}
.brand { font-size: 18px; font-weight: 600; display: flex; align-items: center; gap: 6px; white-space: nowrap; }
.logo { font-size: 20px; }
.header-right { margin-left: auto; }
</style>

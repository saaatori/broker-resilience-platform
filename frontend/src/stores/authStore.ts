import { create } from 'zustand'
import { persist } from 'zustand/middleware'

// 登录态：token 存 localStorage（key brp-auth），刷新不掉线。
// M0 单角色（D2），不存用户信息；用户名展示需要时走 /me。
interface AuthState {
  token: string | null
  setToken: (token: string) => void
  logout: () => void
}

export const useAuthStore = create<AuthState>()(
  persist(
    (set) => ({
      token: null,
      setToken: (token) => set({ token }),
      logout: () => set({ token: null }),
    }),
    { name: 'brp-auth' },
  ),
)

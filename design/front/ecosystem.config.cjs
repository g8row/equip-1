module.exports = {
  apps: [
    {
      name: 'equip-1',
      script: '.output/server/index.mjs',
      instances: 'max',
      exec_mode: 'cluster',
      env: {
        NODE_ENV: 'production',
        PORT: 3234
      }
    }
  ]
}
